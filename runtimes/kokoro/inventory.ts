import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { lstat, readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

export type PackageFile = {
  path: string;
  size: number;
  sha256: string;
  executable: boolean;
};
export type PackageInventory = {
  version: 1;
  target: "macos-arm64" | "linux-x64";
  files: PackageFile[];
};

export function packagePath(value: string): string {
  if (
    !/^[A-Za-z0-9_@.+ /-]+$/.test(value) ||
    value.startsWith("/") ||
    value.split("/").some((part) => !part || part === "." || part === "..")
  ) {
    throw new Error(`Unsafe package path: ${value}`);
  }
  return value;
}

export async function digestFile(path: string): Promise<string> {
  const hash = createHash("sha256");
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest("hex");
}

export async function packageFiles(
  root: string,
  prefix = "",
): Promise<PackageFile[]> {
  const files: PackageFile[] = [];
  for (const name of (await readdir(join(root, prefix))).sort()) {
    const path = packagePath(prefix ? `${prefix}/${name}` : name);
    if (path === "inventory.json") continue;
    const stat = await lstat(join(root, path));
    if (stat.isDirectory()) files.push(...(await packageFiles(root, path)));
    else if (stat.isFile())
      files.push({
        path,
        size: stat.size,
        sha256: await digestFile(join(root, path)),
        executable: Boolean(stat.mode & 0o111),
      });
    else
      throw new Error(
        `Package must not contain links or special files: ${path}`,
      );
  }
  return files;
}

export function parseInventory(value: unknown): PackageInventory {
  if (
    typeof value !== "object" ||
    value === null ||
    !("version" in value) ||
    value.version !== 1 ||
    !("target" in value) ||
    (value.target !== "macos-arm64" && value.target !== "linux-x64") ||
    !("files" in value) ||
    !Array.isArray(value.files) ||
    value.files.length === 0
  ) {
    throw new Error("Invalid Kokoro package inventory.");
  }
  const files: PackageFile[] = [];
  const paths = new Set<string>();
  for (const entry of value.files) {
    if (
      typeof entry !== "object" ||
      entry === null ||
      !("path" in entry) ||
      typeof entry.path !== "string" ||
      !("size" in entry) ||
      typeof entry.size !== "number" ||
      !Number.isSafeInteger(entry.size) ||
      entry.size < 0 ||
      !("sha256" in entry) ||
      typeof entry.sha256 !== "string" ||
      !/^[a-f0-9]{64}$/.test(entry.sha256) ||
      !("executable" in entry) ||
      typeof entry.executable !== "boolean"
    )
      throw new Error("Invalid Kokoro package file.");
    const path = packagePath(entry.path);
    if (path === "inventory.json" || paths.has(path))
      throw new Error("Duplicate or recursive inventory entry.");
    paths.add(path);
    files.push({
      path,
      size: entry.size,
      sha256: entry.sha256,
      executable: entry.executable,
    });
  }
  return { version: 1, target: value.target, files };
}

export async function verifyInventory(
  root: string,
  trustedDigest: string,
): Promise<PackageInventory> {
  if (
    !(await lstat(root)).isDirectory() ||
    !(await lstat(join(root, "inventory.json"))).isFile()
  ) {
    throw new Error("Kokoro package root and inventory must not be links.");
  }
  if (
    !/^[a-f0-9]{64}$/.test(trustedDigest) ||
    (await digestFile(join(root, "inventory.json"))) !== trustedDigest
  ) {
    throw new Error(
      "Kokoro inventory does not match the pinned release digest.",
    );
  }
  const inventory = parseInventory(
    JSON.parse(await readFile(join(root, "inventory.json"), "utf8")),
  );
  const actual = await packageFiles(root);
  const expected = new Map(inventory.files.map((file) => [file.path, file]));
  if (actual.length !== expected.size)
    throw new Error("Kokoro package file set changed.");
  for (const file of actual) {
    const entry = expected.get(file.path);
    if (
      !entry ||
      entry.size !== file.size ||
      entry.sha256 !== file.sha256 ||
      entry.executable !== file.executable
    )
      throw new Error(`Kokoro package integrity failed: ${file.path}`);
  }
  return inventory;
}
