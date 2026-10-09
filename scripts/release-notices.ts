import { readFile, readdir } from "node:fs/promises";
import { join } from "node:path";

type LockPackage = [
  string,
  string,
  {
    dependencies?: Record<string, string>;
    optionalDependencies?: Record<string, string>;
  }?,
];
type Lockfile = {
  workspaces: Record<string, { dependencies?: Record<string, string> }>;
  packages: Record<string, LockPackage>;
};

function packageRecord(lock: Lockfile, name: string): LockPackage | undefined {
  return Object.values(lock.packages).find((value) =>
    value[0].startsWith(`${name}@`),
  );
}

export function productionDependencyClosure(lock: Lockfile): string[] {
  const root = lock.workspaces[""];
  if (!root?.dependencies)
    throw new Error("bun.lock has no root production dependencies.");
  const pending = Object.keys(root.dependencies);
  const included = new Set<string>();
  while (pending.length) {
    const name = pending.pop()!;
    if (included.has(name)) continue;
    included.add(name);
    const record = packageRecord(lock, name);
    if (!record)
      throw new Error(`bun.lock has no production package record for ${name}.`);
    const metadata = record[2];
    pending.push(...Object.keys(metadata?.dependencies ?? {}));
    pending.push(...Object.keys(metadata?.optionalDependencies ?? {}));
  }
  return [...included].sort();
}

async function packageLicenseText(
  packageName: string,
  version: string,
  rootDirectory: string,
): Promise<string> {
  const packageDirectory = join(rootDirectory, "node_modules", packageName);
  const filenames = (await readdir(packageDirectory))
    .filter((filename) =>
      /^(LICENSE|LICENCE|COPYING|NOTICE)([-._]|$)/i.test(filename),
    )
    .sort();
  const texts = await Promise.all(
    filenames.map(async (filename) => {
      const text = (
        await readFile(join(packageDirectory, filename), "utf8")
      ).trim();
      return text ? `${filename}\n${text}` : "";
    }),
  );
  const found = texts.filter(Boolean);
  if (found.length) return found.join("\n\n");
  const fallbackName = packageName.replaceAll("/", "__");
  const fallbackPath = join(
    rootDirectory,
    "scripts",
    "release-notices",
    "licenses",
    `${fallbackName}-${version}.txt`,
  );
  const fallback = Bun.file(fallbackPath);
  if (await fallback.exists()) {
    const text = (await fallback.text()).trim();
    if (text) return `${fallbackPath}\n${text}`;
  }
  throw new Error(
    `No license text found for bundled dependency ${packageName}.`,
  );
}

export async function generateReleaseNotices(
  rootDirectory = ".",
): Promise<string> {
  const lock = Bun.JSONC.parse(
    await Bun.file(join(rootDirectory, "bun.lock")).text(),
  ) as Lockfile;
  const dependencies = productionDependencyClosure(lock);
  const sections = await Promise.all(
    dependencies.map(async (name) => {
      const record = packageRecord(lock, name)!;
      const version = record[0].slice(name.length + 1);
      const license = await packageLicenseText(name, version, rootDirectory);
      return `## ${name}@${version}\n\n${license}`;
    }),
  );

  const packageManager = JSON.parse(
    await Bun.file(join(rootDirectory, "package.json")).text(),
  ).packageManager as string;
  const match = /^bun@(.+)$/.exec(packageManager);
  if (!match)
    throw new Error(`Unsupported packageManager value: ${packageManager}.`);
  const bunVersion = match[1]!;
  const bunNoticesPath = join(
    rootDirectory,
    "scripts",
    "release-notices",
    `bun-${bunVersion}.txt`,
  );
  const bunNoticesFile = Bun.file(bunNoticesPath);
  if (!(await bunNoticesFile.exists())) {
    throw new Error(
      `Missing versioned Bun notices for pinned Bun ${bunVersion}: ${bunNoticesPath}.`,
    );
  }
  const bunNotices = (await bunNoticesFile.text()).trim();

  const runtimes = (await Bun.file(
    join(rootDirectory, "src/manager/managed-runtime-manifest.json"),
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
    "LocalBase is distributed under AGPL-3.0-or-later; see the accompanying LICENSE file.",
    "",
    "The compiled CLI includes the production dependency closure from bun.lock. Full license texts follow.",
    ...sections,
    "",
    bunNotices,
    "",
    "Managed runtime downloads are separately licensed by their upstream projects. Runtime versions and source URLs:",
    ...runtimeLines.map((item) => `- ${item}`),
    "",
  ].join("\n");
}

if (import.meta.main) process.stdout.write(await generateReleaseNotices());
