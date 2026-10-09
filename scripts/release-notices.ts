import { readdir } from "node:fs/promises";
import { join } from "node:path";

type PackageInfo = {
  name?: string;
  version?: string;
  license?: string | { type?: string };
};

async function packageNotices(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { withFileTypes: true });
  const packages: string[] = [];
  for (const entry of entries) {
    if (entry.name.startsWith(".")) continue;
    const path = join(directory, entry.name);
    if (entry.name.startsWith("@") && entry.isDirectory()) {
      packages.push(...(await packageNotices(path)));
    } else if (entry.isDirectory()) {
      const file = Bun.file(join(path, "package.json"));
      if (!(await file.exists())) continue;
      const info = (await file.json()) as PackageInfo;
      const license =
        typeof info.license === "string"
          ? info.license
          : (info.license?.type ?? "SEE PACKAGE");
      packages.push(
        `${info.name ?? entry.name}@${info.version ?? "unknown"} — ${license}`,
      );
    }
  }
  return packages;
}

export async function generateReleaseNotices(): Promise<string> {
  const packages = [...new Set(await packageNotices("node_modules"))].sort();
  const runtimes = (await Bun.file(
    "src/manager/managed-runtime-manifest.json",
  ).json()) as {
    targets: Array<{
      platform: string;
      architecture: string;
      runtimes: Record<string, { tag: string; url: string }>;
    }>;
  };
  const runtimeLines = runtimes.targets
    .flatMap((target) =>
      Object.entries(target.runtimes).map(
        ([name, runtime]) =>
          `${target.platform}-${target.architecture}: ${name} ${runtime.tag} (${runtime.url})`,
      ),
    )
    .sort();
  return [
    "LocalBase third-party notices",
    "",
    "LocalBase is distributed under AGPL-3.0; see the accompanying LICENSE file.",
    "",
    "The compiled CLI embeds JavaScript dependencies. Their package licenses are listed below.",
    ...packages.map((item) => `- ${item}`),
    "",
    "Managed runtime downloads are separately licensed by their upstream projects. Runtime versions and source URLs:",
    ...runtimeLines.map((item) => `- ${item}`),
    "",
  ].join("\n");
}

if (import.meta.main) process.stdout.write(await generateReleaseNotices());
