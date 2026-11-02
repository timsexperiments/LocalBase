import { expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const repository = resolve(import.meta.dir, "..");

function run(command: string, args: string[], cwd: string, env = {}) {
  const result = spawnSync(command, args, {
    cwd,
    encoding: "utf8",
    env: { ...process.env, LOCALBASE_ROOT: undefined, ...env },
  });
  return result;
}

/** A release directory for this machine, and a `curl` that serves it offline. */
function fixture() {
  const work = mkdtempSync(join(tmpdir(), "lb-install-test-"));
  const os = process.platform === "darwin" ? "macos" : "linux";
  const arch = process.arch === "arm64" ? "arm64" : "x64";
  const name = `local-base-${os}-${arch}`;
  const archive = os === "macos" ? `${name}.zip` : `${name}.tar.gz`;
  const release = join(work, "release");
  const bin = join(work, "bin");
  const home = join(work, "home");
  for (const dir of [release, bin, home]) mkdirSync(dir);

  writeFileSync(join(release, name), "#!/bin/sh\necho fake\n");
  chmodSync(join(release, name), 0o755);
  writeFileSync(join(release, "LICENSE"), "license\n");
  writeFileSync(join(release, "THIRD_PARTY_NOTICES.txt"), "notices\n");
  const files = [name, "LICENSE", "THIRD_PARTY_NOTICES.txt"];
  const packed =
    os === "macos"
      ? run("zip", ["-q", archive, ...files], release)
      : run("tar", ["-czf", archive, ...files], release);
  expect(packed.status).toBe(0);
  const sum = createHash("sha256")
    .update(readFileSync(join(release, archive)))
    .digest("hex");
  writeFileSync(join(release, "checksums.txt"), `${sum}  ${archive}\n`);

  writeFileSync(
    join(bin, "curl"),
    `#!/bin/sh\nwhile [ "$#" -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done\ncp "${release}/$(basename "$url")" "$out"\n`,
  );
  chmodSync(join(bin, "curl"), 0o755);

  return {
    work,
    home,
    env: { HOME: home, PATH: `${bin}:${process.env.PATH}` },
  };
}

test("installs license files outside the default data root", () => {
  const { work, home, env } = fixture();
  try {
    const installed = run(
      "sh",
      [join(repository, "scripts/install.sh")],
      work,
      env,
    );
    expect(installed.status).toBe(0);
    expect(existsSync(join(home, ".local/bin/local-base"))).toBe(true);
    expect(existsSync(join(home, ".local/share/doc/local-base/LICENSE"))).toBe(
      true,
    );
    expect(existsSync(join(home, ".local/share/local-base"))).toBe(false);

    const init = run(
      "bun",
      ["run", "src/cli.ts", "--non-interactive", "init"],
      repository,
      env,
    );
    expect(init.stderr).not.toContain("nonempty");
    expect(init.status).toBe(0);
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
});

test("LOCALBASE_LICENSE_DIR still chooses the license destination", () => {
  const { work, env } = fixture();
  const licenses = join(work, "chosen");
  try {
    const installed = run(
      "sh",
      [join(repository, "scripts/install.sh")],
      work,
      {
        ...env,
        LOCALBASE_LICENSE_DIR: licenses,
      },
    );
    expect(installed.status).toBe(0);
    expect(existsSync(join(licenses, "LICENSE"))).toBe(true);
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
});
