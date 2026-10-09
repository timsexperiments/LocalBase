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

export const nativeNoticeManifest: Record<string, string[]> = {
  Bun: ["Bun-MIT.txt"],
  "uWebSockets and uSockets": ["Apache-2.0.txt"],
  "WebKit/JavaScriptCore": ["LGPL-2.1-only.txt"],
  BoringSSL: ["boringssl.txt"],
  Brotli: ["brotli.txt"],
  "c-ares": ["cares.txt"],
  highway: ["highway.txt"],
  libarchive: [
    "libarchive.txt",
    "libarchive-read-compress.c.txt",
    "libarchive-write-compress.c.txt",
  ],
  libdeflate: ["libdeflate.txt"],
  "libjpeg-turbo": ["libjpeg-turbo.txt", "libjpeg-turbo-ijg.txt"],
  libspng: ["libspng.txt"],
  libuv: ["libuv.txt"],
  "lol-html": ["lol-html.txt"],
  "ls-hpack": ["ls-hpack.txt"],
  "ls-qpack": ["ls-qpack.txt"],
  lsquic: ["lsquic.txt", "lsquic-chrome.txt"],
  mimalloc: ["mimalloc.txt"],
  picohttpparser: ["picohttpparser.txt"],
  libwebp: ["libwebp.txt"],
  uucode: ["uucode.txt"],
  simdutf: ["simdutf.txt"],
  libcxxabi: ["libcxxabi.txt"],
  tinycc: ["tinycc.txt"],
  zlib: ["Zlib.txt"],
  zstd: ["zstd.txt"],
  "zlib-ng": ["zlib-ng.txt"],
  ICU: ["ICU-72.txt"],
  "polyfill assert@2.1.0": ["polyfill-assert-2.1.0.txt"],
  "polyfill browserify-zlib@0.2.0": ["polyfill-browserify-zlib-0.2.0.txt"],
  "polyfill buffer@6.0.3": ["polyfill-buffer-6.0.3.txt"],
  "polyfill constants-browserify@1.0.0": [
    "polyfill-constants-browserify-1.0.0.txt",
  ],
  "polyfill crypto-browserify@3.12.1": [
    "polyfill-crypto-browserify-3.12.1.txt",
  ],
  "polyfill domain-browser@4.23.0": ["polyfill-domain-browser-4.23.0.txt"],
  "polyfill events@3.3.0": ["polyfill-events-3.3.0.txt"],
  "polyfill https-browserify@1.0.0": ["polyfill-https-browserify-1.0.0.txt"],
  "polyfill os-browserify@0.3.0": ["polyfill-os-browserify-0.3.0.txt"],
  "polyfill path-browserify@1.0.1": ["polyfill-path-browserify-1.0.1.txt"],
  "polyfill process@0.11.10": ["polyfill-process-0.11.10.txt"],
  "polyfill punycode@2.3.1": ["polyfill-punycode-2.3.1.txt"],
  "polyfill querystring-es3@1.0.0-0": ["polyfill-querystring-es3-1.0.0-0.txt"],
  "polyfill readable-stream@4.7.0": ["polyfill-readable-stream-4.7.0.txt"],
  "polyfill stream-http@3.2.0": ["polyfill-stream-http-3.2.0.txt"],
  "polyfill string_decoder@1.3.0": ["polyfill-string_decoder-1.3.0.txt"],
  "polyfill timers-browserify@2.0.12": [
    "polyfill-timers-browserify-2.0.12.txt",
  ],
  "polyfill tty-browserify@0.0.1": ["polyfill-tty-browserify-0.0.1.txt"],
  "polyfill url@0.11.4": ["polyfill-url-0.11.4.txt"],
  "polyfill util@0.12.5": ["polyfill-util-0.12.5.txt"],
  "polyfill vm-browserify@1.1.2": ["polyfill-vm-browserify-1.1.2.txt"],
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
    .filter((filename) => /^(LICENSE|LICENCE|COPYING)([-._]|$)/i.test(filename))
    .sort();
  const notices = (await readdir(packageDirectory))
    .filter((filename) => /^NOTICE([-._]|$)/i.test(filename))
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
  if (found.length) {
    const supplemental = await Promise.all(
      notices.map(async (filename) => {
        const text = (
          await readFile(join(packageDirectory, filename), "utf8")
        ).trim();
        return text ? `${filename}\n${text}` : "";
      }),
    );
    return [...found, ...supplemental.filter(Boolean)].join("\n\n");
  }
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
  if (!bunNotices)
    throw new Error(`Bun notices for pinned Bun ${bunVersion} are empty.`);
  const nativeLicenseDirectory = join(
    rootDirectory,
    "scripts",
    "release-notices",
    "native",
  );
  let nativeFiles: string[];
  try {
    nativeFiles = await readdir(nativeLicenseDirectory);
  } catch {
    throw new Error("No vendored native license texts found.");
  }
  const nativeLicenseNames = [
    ...new Set(Object.values(nativeNoticeManifest).flat()),
  ];
  if (!nativeLicenseNames.length)
    throw new Error("No vendored native license texts found.");
  for (const filename of nativeLicenseNames) {
    let text: string;
    try {
      text = await readFile(join(nativeLicenseDirectory, filename), "utf8");
    } catch {
      throw new Error("Missing required native license file " + filename + ".");
    }
    if (!text.trim())
      throw new Error(`Vendored native license file ${filename} is empty.`);
  }

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
    "Vendored native component license texts:",
    ...(await Promise.all(
      nativeLicenseNames
        .sort()
        .map(
          async (filename) =>
            `\n### ${filename}\n\n${(await readFile(join(nativeLicenseDirectory, filename), "utf8")).trim()}`,
        ),
    )),
    "",
    "Managed runtime downloads are separately licensed by their upstream projects. Runtime versions and source URLs:",
    ...runtimeLines.map((item) => `- ${item}`),
    "",
  ].join("\n");
}

if (import.meta.main) process.stdout.write(await generateReleaseNotices());
