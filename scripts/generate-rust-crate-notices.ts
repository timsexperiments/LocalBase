import { createHash } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const bunCommit = "744846f844374847c902b5e7fd59b4342a51ef99";

// Registry crates linked into Bun's bun_runtime staticlib at the pin. Union of
// `cargo tree --locked -p bun_runtime -e normal,no-proc-macro --prefix none
// --no-dedupe --target <triple>` for aarch64/x86_64 apple-darwin and
// linux-gnu, excluding in-tree path crates (vendored lol-html and rust-argon2
// have their own notices). Proc-macro and build-only crates are not linked.
const crates = `
allocator-api2 0.2.21; arrayref 0.3.9; arrayvec 0.7.6; base64 0.22.1;
bcrypt 0.19.0; bitflags 2.11.1; blake2 0.10.6; blake2b_simd 1.0.4;
block-buffer 0.10.4; blowfish 0.9.1; bstr 1.12.1; bytemuck 1.25.0;
byteorder 1.5.0; cfg-if 1.0.4; cipher 0.4.4; const_format 0.2.36;
constant_time_eq 0.4.2; cpufeatures 0.2.17; crossbeam-utils 0.8.21;
crypto-common 0.1.7; cssparser 0.36.0; derive_more 2.1.1; digest 0.10.7;
dtoa 1.0.11; dtoa-short 0.3.5; encoding_rs 0.8.35; enum-map 2.7.3;
enumset 1.1.10; equivalent 1.0.2; foldhash 0.2.0; generic-array 0.14.7;
getrandom 0.4.2; hashbrown 0.15.5; hashbrown 0.16.1; inout 0.1.4; itoa 1.0.18;
keccak 0.1.6; konst 0.2.20; konst_macro_rules 0.2.19; libc 0.2.186;
linux-raw-sys 0.4.15; log 0.4.29; memchr 2.8.0; mime 0.3.17;
new_debug_unreachable 1.0.6; phf 0.13.1; phf_shared 0.13.1;
precomputed-hash 0.1.1; rustc-hash 2.1.2; rustix 0.38.44; scopeguard 1.2.0;
selectors 0.33.0; servo_arc 0.4.3; sha3 0.10.9; siphasher 1.0.3;
smallvec 1.15.1; stable_deref_trait 1.2.1; strum 0.26.3; subtle 2.6.1;
thiserror 2.0.18; typed-arena 2.0.2; typenum 1.20.0; zeroize 1.8.2`
  .split(";")
  .map((entry) => entry.trim().split(/\s+/) as [string, string]);

// Crates whose published archive carries no license file. The note records
// where the terms come from; the URL is fetched and reproduced verbatim.
const upstreamLicenses: Record<string, { url: string; note: string }> = {
  "blake2b_simd@1.0.4": {
    url: "https://raw.githubusercontent.com/oconnor663/blake2_simd/6a94ae2e18985c7c35bada8a4d438bfe9ab0d0c0/LICENSE",
    note: "Cargo.toml declares MIT; the archive has no license file. The repository LICENSE at the crate's .cargo_vcs_info.json commit follows.",
  },
  "selectors@0.33.0": {
    url: "https://www.mozilla.org/media/MPL/2.0/index.txt",
    note: "Cargo.toml declares MPL-2.0 and each source file carries the MPL-2.0 Exhibit A header; the archive has no license file. Source Code Form: https://github.com/servo/stylo/tree/f319793c6989dba83994fbd10d560b21ad4a0c85/selectors and https://static.crates.io/crates/selectors/selectors-0.33.0.crate. The MPL-2.0 text follows.",
  },
};

const rustCommit = "9f36de775bc636c8e88c31a173c2bcb6995956a0"; // nightly-2026-07-20
const rustRaw = `https://raw.githubusercontent.com/rust-lang/rust/${rustCommit}`;

// Registry crates statically linked through Rust std for the apple-darwin and
// linux-gnu targets, from std's dependency list in library/Cargo.lock.
// Target-specific crates for other platforms and dev/build-only crates are
// not linked.
const stdCrates = `
addr2line 0.27.0; adler2 2.0.1; cfg-if 1.0.4; gimli 0.34.0; hashbrown 0.17.1;
libc 0.2.185; memchr 2.7.6; miniz_oxide 0.9.1; object 0.39.1;
rustc-demangle 0.1.28`
  .split(";")
  .map((entry) => entry.trim().split(/\s+/) as [string, string]);

async function fetchText(url: string) {
  const response = await fetch(url);
  if (!response.ok)
    throw new Error(`Could not fetch ${url}: ${response.status}.`);
  return response.text();
}

async function crateNotices(
  lockUrl: string,
  list: [string, string][],
  directory: string,
) {
  const checksums = new Map<string, string>();
  for (const block of (await fetchText(lockUrl)).split("[[package]]")) {
    const name = /^name = "([^"]+)"/m.exec(block)?.[1];
    const version = /^version = "([^"]+)"/m.exec(block)?.[1];
    const checksum = /^checksum = "([0-9a-f]{64})"/m.exec(block)?.[1];
    if (name && version && checksum)
      checksums.set(`${name}@${version}`, checksum);
  }
  const sections: string[] = [];
  for (const [name, version] of list) {
    const checksum = checksums.get(`${name}@${version}`);
    if (!checksum)
      throw new Error(`No Cargo.lock checksum for ${name}@${version}.`);
    const response = await fetch(
      `https://static.crates.io/crates/${name}/${name}-${version}.crate`,
    );
    if (!response.ok)
      throw new Error(
        `Failed to fetch ${name}@${version}: ${response.status}.`,
      );
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (createHash("sha256").update(bytes).digest("hex") !== checksum)
      throw new Error(`Checksum mismatch for ${name}@${version}.`);
    const filename = join(directory, "crate.tgz");
    await Bun.write(filename, bytes);
    const listing = Bun.spawnSync(["tar", "-tzf", filename]);
    if (listing.exitCode !== 0)
      throw new Error(`Could not list ${name}@${version}.`);
    const root = `${name}-${version}/`;
    const pattern = new RegExp(
      `^${root.replace(/[.+]/g, "\\$&")}(LICEN[CS]ES/[^/]+|(LICEN[CS]E|COPYING|COPYRIGHT|NOTICE|UNLICENSE)([-._][^/]*)?)$`,
      "i",
    );
    const licenseFiles = listing.stdout
      .toString()
      .split("\n")
      .filter((path) => pattern.test(path))
      .sort();
    let text = `\n\n===== ${name}@${version} (crates.io sha256 ${checksum}) =====\n`;
    if (!licenseFiles.length) {
      const upstream = upstreamLicenses[`${name}@${version}`];
      if (!upstream) throw new Error(`No license file in ${name}@${version}.`);
      text += `\n${upstream.note}\n\n--- ${upstream.url} ---\n${await fetchText(upstream.url)}`;
      sections.push(text);
      continue;
    }
    for (const path of licenseFiles) {
      const extracted = Bun.spawnSync(["tar", "-xzOf", filename, path]);
      if (extracted.exitCode !== 0)
        throw new Error(`Could not extract ${path} from ${name}@${version}.`);
      text += `\n--- ${path.slice(root.length)} ---\n${extracted.stdout.toString()}`;
    }
    sections.push(text);
  }
  return sections.join("");
}

const directory = await mkdtemp(join(tmpdir(), "bun-rust-crate-notices-"));
try {
  const bunLockUrl = `https://raw.githubusercontent.com/oven-sh/bun/${bunCommit}/Cargo.lock`;
  await Bun.write(
    "scripts/release-notices/native/rust-crates-closure.txt",
    `Generated from the crates.io packages pinned by ${bunLockUrl}. Each crate's license files are reproduced from its checksum-verified .crate archive.\n${await crateNotices(bunLockUrl, crates, directory)}`,
  );
  const stdLockUrl = `${rustRaw}/library/Cargo.lock`;
  let std = `Rust standard library (core, alloc, std and their in-tree runtime crates) from rust-lang/rust ${rustCommit} (nightly-2026-07-20, pinned by Bun's rust-toolchain.toml), statically linked into Bun's Rust runtime.\n`;
  for (const path of [
    "COPYRIGHT",
    "LICENSE-MIT",
    "LICENSE-APACHE",
    "library/compiler-builtins/LICENSE.txt",
  ])
    std += `\n--- ${rustRaw}/${path} ---\n${await fetchText(`${rustRaw}/${path}`)}`;
  std += `\n\nRegistry crates linked through std, from ${stdLockUrl}:\n${await crateNotices(stdLockUrl, stdCrates, directory)}`;
  await Bun.write("scripts/release-notices/native/rust-std.txt", std);
} finally {
  await rm(directory, { recursive: true, force: true });
}
