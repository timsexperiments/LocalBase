import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { packageClosure, type Lock } from "./bun-lock-closure";
import { readmeLicenseSection } from "./package-readme-license";

const bunCommit = "0d9b296af33f2b851fcbf4df3e9ec89751734ba4";
const lockUrl = `https://raw.githubusercontent.com/oven-sh/bun/${bunCommit}/src/node-fallbacks/bun.lock`;

const directory = await mkdtemp(join(tmpdir(), "bun-fallback-notices-"));
const sections: string[] = [];
try {
  const lockResponse = await fetch(lockUrl);
  if (!lockResponse.ok)
    throw new Error(`Could not fetch Bun lock: ${lockResponse.status}.`);
  const lock = Bun.JSONC.parse(await lockResponse.text()) as Lock;
  const records = packageClosure(lock);
  for (const record of records) {
    const name = record[0].slice(0, record[0].lastIndexOf("@"));
    const version = record[0].slice(name.length + 1);
    const integrity = record[3];
    if (!integrity?.startsWith("sha512-"))
      throw new Error(`Missing SHA-512 integrity for ${name}@${version}.`);
    const filename = join(directory, "package.tgz");
    const packageName = name.split("/").at(-1)!;
    const response = await fetch(
      `https://registry.npmjs.org/${name.replace("/", "%2f")}/-/${packageName}-${version}.tgz`,
    );
    if (!response.ok)
      throw new Error(
        `Failed to fetch ${name}@${version}: ${response.status}.`,
      );
    const bytes = new Uint8Array(await response.arrayBuffer());
    const actual = `sha512-${Buffer.from(createHash("sha512").update(bytes).digest()).toString("base64")}`;
    if (actual !== integrity)
      throw new Error(`Integrity mismatch for ${name}@${version}.`);
    await Bun.write(filename, bytes);
    const listing = Bun.spawnSync(["tar", "-tzf", filename]);
    if (listing.exitCode !== 0)
      throw new Error(`Could not list ${name}@${version}.`);
    const licenseFiles = listing.stdout
      .toString()
      .split("\n")
      .filter((path) =>
        /^package\/(LICENSE|LICENCE|COPYING)([-._]|$)|^package\/NOTICE([-._]|$)/i.test(
          path,
        ),
      )
      .filter((path) => !path.endsWith("/"))
      .sort();
    const readmeFiles = listing.stdout
      .toString()
      .split("\n")
      .filter((path) => /^package\/(README|Readme)(\.[^/]*)?$/i.test(path))
      .filter((path) => !path.endsWith("/"))
      .sort();
    let text = `\n\n===== ${name}@${version} (npm integrity ${integrity}) =====\n`;
    for (const path of licenseFiles) {
      const extracted = Bun.spawnSync(["tar", "-xzOf", filename, path]);
      if (extracted.exitCode !== 0)
        throw new Error(`Could not extract ${path} from ${name}@${version}.`);
      text += `\n--- ${path.slice("package/".length)} ---\n${extracted.stdout.toString()}`;
    }
    if (!licenseFiles.length) {
      let readmeSection: string | undefined;
      for (const path of readmeFiles) {
        const extracted = Bun.spawnSync(["tar", "-xzOf", filename, path]);
        if (extracted.exitCode !== 0)
          throw new Error(`Could not extract ${path} from ${name}@${version}.`);
        readmeSection = readmeLicenseSection(extracted.stdout.toString());
        if (readmeSection) {
          text += `\nLicense section from ${path} in the verified package tarball follows verbatim.\n\n${readmeSection}\n`;
          break;
        }
      }
      if (readmeSection) {
        sections.push(text);
        continue;
      }
      const metadataResponse = await fetch(
        `https://registry.npmjs.org/${name.replace("/", "%2f")}/${version}`,
      );
      if (!metadataResponse.ok)
        throw new Error(`Could not read metadata for ${name}@${version}.`);
      const metadata = (await metadataResponse.json()) as { license?: string };
      if (metadata.license !== "MIT")
        throw new Error(
          `No license file or known MIT license for ${name}@${version}.`,
        );
      const mitTerms = await readFile(
        "scripts/release-notices/native/Bun-MIT.txt",
        "utf8",
      );
      text += `\nNpm package metadata declares MIT; its verified tarball has no LICENSE/NOTICE file. Standard MIT terms follow. No unavailable copyright holder is inferred.\n\n${mitTerms}\n`;
    }
    sections.push(text);
  }
  await Bun.write(
    "scripts/release-notices/native/polyfill-fallback-closure.txt",
    `Generated from ${lockUrl}. Build-only esbuild and react-refresh packages are excluded.\n${sections.join("")}`,
  );
} finally {
  await rm(directory, { recursive: true, force: true });
}
