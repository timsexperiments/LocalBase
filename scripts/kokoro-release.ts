import { createHash } from "node:crypto";
import {
  cp,
  lstat,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  unlink,
  writeFile,
} from "node:fs/promises";
import { basename, dirname, join, relative, resolve } from "node:path";
import { Readable } from "node:stream";
import { createGunzip } from "node:zlib";
import { unzipSync } from "fflate";
import { extract as tarExtractor } from "tar-stream";
import { z } from "zod";
import {
  digestFile,
  packageFiles,
  packagePath,
  verifyInventory,
} from "../runtimes/kokoro/inventory";
import sources from "../runtimes/kokoro/sources.json";
import { readSdArchiveEntries } from "./sd-release/archive-reader";
import { repositorySchema } from "./whisper-release/contracts";
import {
  archiveName,
  kokoroArtifactSchema,
  kokoroInvocation,
  kokoroReleaseManifestSchema,
  kokoroTagSchema,
  kokoroTargetSchema,
  type KokoroArtifact,
  type KokoroTarget,
} from "./kokoro-package-contract";

const runtimeSource = resolve(import.meta.dir, "../runtimes/kokoro");
const lockSchema = z
  .object({
    packages: z.record(
      z.string(),
      z.tuple([
        z.string(),
        z.string(),
        z.unknown(),
        z.string().regex(/^sha512-[A-Za-z0-9+/]+=*$/),
      ]),
    ),
  })
  .passthrough();
const metadataSchema = z
  .object({
    name: z.string(),
    version: z.string(),
    license: z.unknown().optional(),
  })
  .passthrough();

async function command(
  args: string[],
  cwd?: string,
  env?: Record<string, string>,
): Promise<string> {
  const child = Bun.spawn(args, {
    cwd,
    env,
    stdout: "pipe",
    stderr: "inherit",
  });
  const output = await new Response(child.stdout).text();
  if ((await child.exited) !== 0)
    throw new Error(`Packaging command failed: ${args[0]}`);
  return output;
}

async function bytes(url: string): Promise<Uint8Array> {
  const response = await fetch(url);
  if (!response.ok)
    throw new Error(`Asset download failed: ${response.status} ${url}`);
  return new Uint8Array(await response.arrayBuffer());
}

function nativeTarget(target: KokoroTarget): void {
  const valid =
    target === "macos-arm64"
      ? process.platform === "darwin" && process.arch === "arm64"
      : process.platform === "linux" && process.arch === "x64";
  if (!valid)
    throw new Error(`Packaging ${target} requires its native runner.`);
}

async function verifyBun(target: KokoroTarget, root: string): Promise<void> {
  const path = join(root, "kokoro-tts");
  const pin = sources.targets[target];
  const info = await lstat(path);
  if (
    !info.isFile() ||
    info.size !== pin.executableSize ||
    (info.mode & 0o111) === 0 ||
    (await digestFile(path)) !== pin.executableSha256
  )
    throw new Error("Packaged Bun prebuilt integrity failed.");
}

export async function preparePackage(
  target: KokoroTarget,
  directory: string,
): Promise<void> {
  nativeTarget(target);
  const root = resolve(directory);
  await mkdir(root, { recursive: true });
  if ((await readdir(root)).length !== 0)
    throw new Error("Package preparation requires an empty directory.");
  const pin = sources.targets[target];
  const archive = await bytes(pin.url);
  if (
    archive.length !== pin.size ||
    createHash("sha256").update(archive).digest("hex") !== pin.sha256
  ) {
    throw new Error("Official Bun asset does not match its immutable pin.");
  }
  const executable = unzipSync(archive)[pin.member];
  if (!executable) throw new Error("Official Bun executable is missing.");
  if (
    executable.length !== pin.executableSize ||
    createHash("sha256").update(executable).digest("hex") !==
      pin.executableSha256
  ) {
    throw new Error("Official Bun member does not match its executable pin.");
  }
  await writeFile(join(root, "kokoro-tts"), executable, { mode: 0o755 });
  await cp(join(runtimeSource, "package.json"), join(root, "package.json"));
  await cp(join(runtimeSource, "bun.lock"), join(root, "bun.lock"));
  await cp(join(runtimeSource, "bunfig.toml"), join(root, "bunfig.toml"));
  const version = await command([join(root, "kokoro-tts"), "--version"]);
  if (version.trim() !== sources.bunVersion)
    throw new Error("Pinned Bun version mismatch.");
}

export async function installDependencies(
  target: KokoroTarget,
  directory: string,
): Promise<void> {
  nativeTarget(target);
  const root = resolve(directory);
  // Package prebuilts are already in registry tarballs. No lifecycle/build/CUDA fallback.
  await verifyBun(target, root);
  await cp(join(runtimeSource, "bun.lock"), join(root, "bun.lock"));
  await command(
    [
      join(root, "kokoro-tts"),
      "install",
      "--frozen-lockfile",
      "--ignore-scripts",
    ],
    root,
    {
      PATH: dirname(process.execPath),
      HOME: process.env.HOME ?? root,
      ONNXRUNTIME_NODE_INSTALL_CUDA: "skip",
    },
  );
  if (
    (await digestFile(join(root, "bun.lock"))) !==
    (await digestFile(join(runtimeSource, "bun.lock")))
  ) {
    throw new Error("Runtime lock changed during dependency installation.");
  }
  await removeBinLinks(join(root, "node_modules"));
}

async function removeBinLinks(directory: string): Promise<void> {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.name === ".bin" && entry.isDirectory()) {
      for (const name of await readdir(path)) {
        const link = join(path, name);
        if (!(await lstat(link)).isSymbolicLink())
          throw new Error("Unexpected dependency bin entry.");
        await unlink(link);
      }
    } else if (entry.isDirectory()) await removeBinLinks(path);
  }
}

type LockedPackage = {
  root: string;
  name: string;
  version: string;
  integrity: string;
  license: unknown;
};
async function installedPackages(root: string): Promise<LockedPackage[]> {
  const lock = lockSchema.parse(
    Bun.JSONC.parse(await readFile(join(runtimeSource, "bun.lock"), "utf8")),
  );
  const result: LockedPackage[] = [];
  const visit = async (directory: string): Promise<void> => {
    for (const entry of await readdir(directory, { withFileTypes: true })) {
      if (
        entry.name === ".bin" &&
        entry.isDirectory() &&
        (await readdir(join(directory, entry.name))).length === 0
      )
        continue;
      if (entry.name.startsWith("."))
        throw new Error(`Unexpected hidden dependency: ${entry.name}`);
      const path = join(directory, entry.name);
      if (!entry.isDirectory())
        throw new Error(`Unexpected dependency link/file: ${path}`);
      if (entry.name.startsWith("@")) {
        await visit(path);
        continue;
      }
      const metadata = metadataSchema.parse(
        JSON.parse(await readFile(join(path, "package.json"), "utf8")),
      );
      const packageName = basename(directory).startsWith("@")
        ? `${basename(directory)}/${entry.name}`
        : entry.name;
      if (metadata.name !== packageName)
        throw new Error(
          `Dependency directory does not match its package identity: ${path}`,
        );
      const identities = Object.values(lock.packages).filter(
        ([identity]) => identity === `${metadata.name}@${metadata.version}`,
      );
      const identity = identities[0];
      if (!identity || identities.some((other) => other[3] !== identity[3]))
        throw new Error(`Unlocked dependency: ${metadata.name}`);
      result.push({
        root: path,
        name: metadata.name,
        version: metadata.version,
        integrity: identity[3],
        license: metadata.license,
      });
      const nested = join(path, "node_modules");
      if (await Bun.file(join(nested, "package.json")).exists())
        throw new Error("Unexpected nested package root.");
      try {
        if ((await lstat(nested)).isDirectory()) await visit(nested);
      } catch (error) {
        if (
          !(error instanceof Error) ||
          !("code" in error) ||
          error.code !== "ENOENT"
        )
          throw error;
      }
    }
  };
  await visit(join(root, "node_modules"));
  return result;
}

/** Compare every installed package file with its SRI-verified published tarball. */
export async function verifyRegistryPackage(
  pkg: LockedPackage,
  archive: Uint8Array,
): Promise<void> {
  if (
    `sha512-${createHash("sha512").update(archive).digest("base64")}` !==
    pkg.integrity
  ) {
    throw new Error(`Registry package integrity failed: ${pkg.name}`);
  }
  const extractor = tarExtractor();
  const paths = new Set<string>();
  let archiveRoot: string | undefined;
  const completion = new Promise<void>((resolveCompletion, reject) => {
    extractor.once("finish", resolveCompletion);
    extractor.once("error", reject);
    extractor.on("entry", (header, stream, next) => {
      void (async () => {
        const normalized = header.name.replace(/\/$/, "");
        packagePath(normalized);
        const slash = normalized.indexOf("/");
        const root = slash === -1 ? normalized : normalized.slice(0, slash);
        archiveRoot ??= root;
        if (root !== archiveRoot)
          throw new Error(
            "Registry package must have exactly one archive root.",
          );
        if (header.type === "directory") {
          stream.resume();
          next();
          return;
        }
        if (header.type !== "file" || slash === -1)
          throw new Error(
            `Unexpected registry package entry: ${pkg.name}: ${header.type}: ${header.name}`,
          );
        const path = packagePath(normalized.slice(slash + 1));
        if (paths.has(path))
          throw new Error("Duplicate registry package entry.");
        paths.add(path);
        const hash = createHash("sha256");
        let size = 0;
        for await (const chunk of stream) {
          hash.update(chunk);
          size += chunk.length;
        }
        const local = join(pkg.root, path);
        const info = await lstat(local);
        if (
          !info.isFile() ||
          info.size !== size ||
          Boolean(info.mode & 0o111) !== Boolean((header.mode ?? 0) & 0o111) ||
          (await digestFile(local)) !== hash.digest("hex")
        ) {
          throw new Error(
            `Installed prebuilt/package file differs from registry: ${pkg.name}/${path}`,
          );
        }
        next();
      })().catch(next);
    });
  });
  const decompressed = createGunzip();
  decompressed.once("error", (error) => extractor.destroy(error));
  Readable.from([archive]).pipe(decompressed).pipe(extractor);
  await completion;
  const localPaths = (await packageFiles(pkg.root)).filter(
    (file) => !file.path.startsWith("node_modules/"),
  );
  if (
    localPaths.length !== paths.size ||
    localPaths.some((file) => !paths.has(file.path))
  ) {
    throw new Error(`Unexpected installed dependency files: ${pkg.name}`);
  }
}

export async function verifyDependencies(
  target: KokoroTarget,
  directory: string,
): Promise<void> {
  nativeTarget(target);
  const packages = await installedPackages(resolve(directory));
  await verifyBun(target, resolve(directory));
  for (const pkg of packages) {
    const shortName = pkg.name.split("/").at(-1);
    await verifyRegistryPackage(
      pkg,
      await bytes(
        `https://registry.npmjs.org/${pkg.name}/-/${shortName}-${pkg.version}.tgz`,
      ),
    );
  }
  const nativeDirectory =
    target === "macos-arm64" ? "darwin/arm64" : "linux/x64";
  const nativeRoot = join(
    resolve(directory),
    "node_modules/onnxruntime-node/bin/napi-v3",
    nativeDirectory,
  );
  for (const file of [
    "onnxruntime_binding.node",
    target === "macos-arm64"
      ? "libonnxruntime.1.21.0.dylib"
      : "libonnxruntime.so.1.21.0",
  ]) {
    if (!(await lstat(join(nativeRoot, file))).isFile())
      throw new Error(`Missing required prebuilt: ${file}`);
  }
}

export async function packageRuntime(
  target: KokoroTarget,
  directory: string,
  output: string,
): Promise<KokoroArtifact> {
  nativeTarget(target);
  if (Bun.version !== sources.bunVersion)
    throw new Error(
      "Package sidecar must be emitted by the pinned Bun version.",
    );
  const root = resolve(directory);
  // Recheck at packaging, so a separate verification step cannot become stale.
  await verifyDependencies(target, root);
  const result = await Bun.build({
    entrypoints: [join(runtimeSource, "cli.ts")],
    target: "bun",
    outdir: root,
    external: [
      "kokoro-js",
      "@huggingface/transformers",
      "onnxruntime-node",
      "phonemizer",
    ],
    naming: "cli.js",
  });
  if (!result.success)
    throw new Error(`Kokoro sidecar build failed: ${result.logs.join("\n")}`);
  await cp(join(runtimeSource, "licenses"), join(root, "licenses"), {
    recursive: true,
  });
  await cp(join(runtimeSource, "sources.json"), join(root, "sources.json"));
  const packages = await installedPackages(root);
  await writeFile(
    join(root, "licenses/dependencies.json"),
    JSON.stringify(
      packages.map((pkg) => ({
        name: pkg.name,
        version: pkg.version,
        integrity: pkg.integrity,
        license: pkg.license,
        // Published packages and their license/notice files are delivered unchanged.
        files: relative(root, pkg.root),
      })),
      null,
      2,
    ),
  );
  const files = await packageFiles(root);
  for (const [voice, sha256] of [
    [
      "af_heart",
      "d583ccff3cdca2f7fae535cb998ac07e9fcb90f09737b9a41fa2734ec44a8f0b",
    ],
    [
      "af_bella",
      "f69d836209b78eb8c66e75e3cda491e26ea838a3674257e9d4e5703cbaf55c8b",
    ],
  ]) {
    const file = files.find(
      (entry) => entry.path === `node_modules/kokoro-js/voices/${voice}.bin`,
    );
    if (file?.size !== 522240 || file.sha256 !== sha256)
      throw new Error("Pinned voice integrity failed.");
  }
  await writeFile(
    join(root, "inventory.json"),
    `${JSON.stringify({ version: 1, target, files }, null, 2)}\n`,
  );
  const inventorySha256 = await digestFile(join(root, "inventory.json"));
  await verifyInventory(root, inventorySha256);
  await mkdir(resolve(output), { recursive: true });
  const archive = join(resolve(output), archiveName(target));
  await command(["tar", "-czf", archive, "-C", root, ...(await readdir(root))]);
  const artifact = kokoroArtifactSchema.parse({
    target,
    assetName: archiveName(target),
    expectedSizeBytes: (await lstat(archive)).size,
    sha256: await digestFile(archive),
    inventorySha256,
    format: "tar.gz",
    stripComponents: 0,
  });
  await writeFile(
    join(resolve(output), `${target}.json`),
    `${JSON.stringify(artifact, null, 2)}\n`,
  );
  return artifact;
}

async function verifyArchive(
  directory: string,
  artifact: KokoroArtifact,
): Promise<string> {
  const path = join(resolve(directory), artifact.assetName);
  if (
    (await lstat(path)).size !== artifact.expectedSizeBytes ||
    (await digestFile(path)) !== artifact.sha256
  ) {
    throw new Error("Kokoro archive does not match its release manifest.");
  }
  return path;
}

export async function qualifyPackage(
  target: KokoroTarget,
  directory: string,
): Promise<void> {
  nativeTarget(target);
  const artifact = kokoroArtifactSchema.parse(
    JSON.parse(
      await readFile(join(resolve(directory), `${target}.json`), "utf8"),
    ),
  );
  if (artifact.target !== target)
    throw new Error("Qualification target mismatch.");
  const archive = await verifyArchive(directory, artifact);
  const paths = new Set<string>();
  for (const entry of await readSdArchiveEntries(
    "linux-x64",
    await Bun.file(archive).bytes(),
  )) {
    const path = packagePath(entry.name.replace(/\/$/, ""));
    if (
      paths.has(path) ||
      (entry.type !== "file" && entry.type !== "directory")
    ) {
      throw new Error(
        "Kokoro archive contains duplicate paths, links or special files.",
      );
    }
    paths.add(path);
  }
  const temporary = await mkdtemp(
    join(process.env.RUNNER_TEMP ?? "/tmp", "kokoro-relocated-"),
  );
  try {
    const root = join(temporary, "package");
    const privateDirectory = join(temporary, "private");
    await mkdir(root, { mode: 0o700 });
    await mkdir(privateDirectory, { mode: 0o700 });
    await mkdir(join(privateDirectory, "empty-path"));
    await command(["tar", "-xzf", archive, "-C", root]);
    await verifyInventory(root, artifact.inventorySha256);
    const invocation = await kokoroInvocation({
      packageDirectory: root,
      inventorySha256: artifact.inventorySha256,
      arguments: ["smoke"],
      privateDirectory,
    });
    // Runner-only OS sandbox proves the relocated executable cannot use networking.
    const uid = process.getuid?.();
    const gid = process.getgid?.();
    if (uid === undefined || gid === undefined)
      throw new Error("Offline qualification requires a Unix runner.");
    const offline =
      target === "macos-arm64"
        ? [
            "/usr/bin/sandbox-exec",
            "-p",
            "(version 1)(allow default)(deny network*)",
          ]
        : [
            "/usr/bin/sudo",
            "/usr/bin/unshare",
            "--net",
            `--setgid=${gid}`,
            `--setuid=${uid}`,
            "--",
            "/usr/bin/env",
            "-i",
            ...Object.entries(invocation.env).map(
              ([key, value]) => `${key}=${value}`,
            ),
          ];
    await command(
      [...offline, ...invocation.command],
      invocation.cwd,
      invocation.env,
    );
    // Bind the successful relocated smoke to the archive later staged for release.
    await writeFile(
      join(resolve(directory), `${target}.qualified`),
      artifact.sha256,
    );
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

export async function stageRelease(
  directory: string,
  tagInput: unknown,
): Promise<void> {
  const tag = kokoroTagSchema.parse(tagInput);
  const root = resolve(directory);
  const entries: [KokoroTarget, KokoroArtifact][] = [];
  for (const target of kokoroTargetSchema.options) {
    const artifact = kokoroArtifactSchema.parse(
      JSON.parse(await readFile(join(root, `${target}.json`), "utf8")),
    );
    if (artifact.target !== target) throw new Error("Staging target mismatch.");
    await verifyArchive(root, artifact);
    if (
      (await readFile(join(root, `${target}.qualified`), "utf8")) !==
      artifact.sha256
    )
      throw new Error("Archive was not qualified.");
    entries.push([target, artifact]);
  }
  const manifest = kokoroReleaseManifestSchema.parse({
    version: 1,
    tag,
    runtimes: Object.fromEntries(entries),
  });
  await writeFile(
    join(root, "kokoro-runtime-manifest.json"),
    `${JSON.stringify(manifest, null, 2)}\n`,
  );
  await writeFile(
    join(root, "checksums.txt"),
    `${entries.map(([, entry]) => `${entry.sha256}  ${entry.assetName}`).join("\n")}\n`,
  );
}

function githubHeaders(): HeadersInit {
  const token = process.env.GH_TOKEN ?? process.env.GITHUB_TOKEN;
  return {
    Accept: "application/vnd.github+json",
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  };
}

export async function assertUnpublished(
  repositoryInput: unknown,
  tagInput: unknown,
): Promise<void> {
  const repository = repositorySchema.parse(repositoryInput);
  const tag = kokoroTagSchema.parse(tagInput);
  for (const path of [`releases/tags/${tag}`, `git/ref/tags/${tag}`]) {
    const response = await fetch(
      `https://api.github.com/repos/${repository}/${path}`,
      { headers: githubHeaders() },
    );
    if (response.status === 404) continue;
    if (!response.ok)
      throw new Error(`Immutable release check failed: ${response.status}`);
    throw new Error(
      `Kokoro tag ${tag} already exists; republishing is prohibited.`,
    );
  }
}

async function main(): Promise<void> {
  const [operation, ...args] = Bun.argv.slice(2);
  const options = new Map<string, string>();
  for (let index = 0; index < args.length; index += 2) {
    const key = args[index];
    const value = args[index + 1];
    if (!key?.startsWith("--") || !value || options.has(key))
      throw new Error("Expected unique CLI option/value pairs.");
    options.set(key, value);
  }
  if (operation === "validate-tag") {
    kokoroTagSchema.parse(options.get("--tag"));
    return;
  }
  if (operation === "assert-unpublished") {
    await assertUnpublished(options.get("--repository"), options.get("--tag"));
    return;
  }
  const directory = z.string().min(1).parse(options.get("--directory"));
  if (operation === "stage") {
    await stageRelease(directory, options.get("--tag"));
    return;
  }
  const target = kokoroTargetSchema.parse(options.get("--target"));
  if (operation === "prepare") await preparePackage(target, directory);
  else if (operation === "install")
    await installDependencies(target, directory);
  else if (operation === "verify-dependencies")
    await verifyDependencies(target, directory);
  else if (operation === "package")
    await packageRuntime(
      target,
      directory,
      z.string().min(1).parse(options.get("--output")),
    );
  else if (operation === "qualify") await qualifyPackage(target, directory);
  else throw new Error("Unknown Kokoro packaging operation.");
}

if (import.meta.main)
  main().catch((error: unknown) => {
    console.error(error);
    process.exitCode = 1;
  });
