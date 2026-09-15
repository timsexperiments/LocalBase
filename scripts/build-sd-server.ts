import { $ } from "bun";
import { dirname, join } from "node:path";

type SourceArchive = {
  name: string;
  revision: string;
  sha256: string;
  url: string;
  destination: string;
};

const SD_REVISION = "07a85c74cb08cda3aa176f688c5d8f522615e2b9";
const sources: SourceArchive[] = [
  {
    name: "stable-diffusion.cpp",
    revision: SD_REVISION,
    sha256: "a1850648d5fd6e12b23d8f290023563c7dad74ec64302d9ce6862ab880f3d819",
    url: `https://codeload.github.com/leejet/stable-diffusion.cpp/tar.gz/${SD_REVISION}`,
    destination: ".",
  },
  {
    name: "ggml",
    revision: "e20c3a14aa70ee84ca58499814206dd08d8026bc",
    sha256: "3dde7c76c0dc2bce436ab16258baafbf3f1a1dbf933a3583ca32471aadb0e160",
    url: "https://codeload.github.com/leejet/ggml/tar.gz/e20c3a14aa70ee84ca58499814206dd08d8026bc",
    destination: "ggml",
  },
  {
    name: "libwebp",
    revision: "0c9546f7efc61eac7f79ae115c3f99c91c21c443",
    sha256: "6cb433070b4461179067b0901a682e4e14b220354fc35884c1b1315adedefc99",
    url: "https://codeload.github.com/webmproject/libwebp/tar.gz/0c9546f7efc61eac7f79ae115c3f99c91c21c443",
    destination: "thirdparty/libwebp",
  },
  {
    name: "libwebm",
    revision: "5bf12267eea773a32fcf4949de52b0add158a8d5",
    sha256: "294049a03d35e4480a94a5ced96c80ebfd4114554966709c8c97de4f19bd941a",
    url: "https://codeload.github.com/webmproject/libwebm/tar.gz/5bf12267eea773a32fcf4949de52b0add158a8d5",
    destination: "thirdparty/libwebm",
  },
];

async function sha256(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

async function download(source: SourceArchive): Promise<Uint8Array> {
  const response = await fetch(source.url);
  if (!response.ok) {
    throw new Error(
      `Failed to download ${source.name}: HTTP ${response.status}.`,
    );
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  const digest = await sha256(bytes);
  if (digest !== source.sha256) {
    throw new Error(
      `${source.name} source checksum mismatch: expected ${source.sha256}, received ${digest}.`,
    );
  }
  return bytes;
}

async function extract(
  archive: Uint8Array,
  source: SourceArchive,
  root: string,
  sourcePath: string,
) {
  const archivePath = join(root, `${source.name}.tar.gz`);
  const extractionPath = join(root, `extract-${source.name}`);
  await $`mkdir -p ${extractionPath}`;
  await Bun.write(archivePath, archive);
  await $`tar --extract --gzip --file ${archivePath} --directory ${extractionPath}`;
  const extracted = join(extractionPath, `${source.name}-${source.revision}`);
  if (source.destination === ".") {
    await $`mv ${extracted} ${sourcePath}`;
    return;
  }
  const destination = join(sourcePath, source.destination);
  await $`mkdir -p ${dirname(destination)}`;
  await $`mv ${extracted} ${destination}`;
}

async function main() {
  const workspace = process.env.GITHUB_WORKSPACE;
  if (process.env.GITHUB_ACTIONS !== "true" || !workspace) {
    throw new Error("sd-server source builds run only in GitHub Actions.");
  }

  const root = join(workspace, "build-sd-server");
  const sourcePath = join(root, `stable-diffusion.cpp-${SD_REVISION}`);
  const buildPath = join(root, "build");
  await $`rm -rf ${root}`;
  await $`mkdir -p ${root}`;

  for (const source of sources) {
    const archive = await download(source);
    await extract(archive, source, root, sourcePath);
  }

  const patch = join(workspace, "scripts/sd-patches/inline-wav-audio.patch");
  const patchBytes = new Uint8Array(await Bun.file(patch).arrayBuffer());
  await $`git -C ${sourcePath} init --quiet`;
  await $`git -C ${sourcePath} apply --check ${patch}`;
  await $`git -C ${sourcePath} apply ${patch}`;

  const parserTest = join(
    workspace,
    "scripts/sd-patches/inline-wav-audio.test.cpp",
  );
  const parserTestBinary = join(root, "inline-wav-audio-test");
  await $`c++ -std=c++17 -Wall -Wextra -Werror -I${join(sourcePath, "include")} -I${join(sourcePath, "examples")} -I${join(sourcePath, "examples/server")} -I${join(sourcePath, "thirdparty")} ${join(sourcePath, "examples/server/inline_wav.cpp")} ${parserTest} -o ${parserTestBinary}`;
  await $`${parserTestBinary}`;

  const platformFlags =
    process.platform === "linux"
      ? ["-DSD_VULKAN=ON", "-DGGML_NATIVE=OFF"]
      : ["-DSD_METAL=ON", "-DGGML_NATIVE=OFF"];
  await $`cmake -S ${sourcePath} -B ${buildPath} -DCMAKE_BUILD_TYPE=Release -DSD_BUILD_EXAMPLES=ON -DSD_SERVER_BUILD_FRONTEND=OFF -DSD_BUILD_SHARED_LIBS=OFF -DSD_BUILD_SHARED_GGML_LIB=OFF -DSD_WEBP=ON -DSD_WEBM=ON ${platformFlags}`;
  await $`cmake --build ${buildPath} --config Release --target sd-server --parallel`;

  const binary = join(buildPath, "bin", "sd-server");
  if (!(await Bun.file(binary).exists())) {
    throw new Error("The native build completed without sd-server.");
  }
  await Bun.write(join(root, "sd-server"), Bun.file(binary));
  await $`chmod +x ${join(root, "sd-server")}`;

  const licenses: Array<[string, string]> = [
    [join(sourcePath, "LICENSE"), "LICENSE.stable-diffusion.cpp.txt"],
    [join(sourcePath, "ggml/LICENSE"), "LICENSE.ggml.txt"],
    [join(sourcePath, "thirdparty/libwebp/COPYING"), "LICENSE.libwebp.txt"],
    [join(sourcePath, "thirdparty/libwebm/LICENSE.TXT"), "LICENSE.libwebm.txt"],
    [
      join(sourcePath, "thirdparty/utf8proc/LICENSE.md"),
      "LICENSE.utf8proc.txt",
    ],
    [join(sourcePath, "thirdparty/oniguruma/COPYING"), "LICENSE.oniguruma.txt"],
    [
      join(workspace, "scripts/sd-licenses/LICENSE.nlohmann-json.txt"),
      "LICENSE.nlohmann-json.txt",
    ],
    [
      join(sourcePath, "thirdparty/LICENSE.darts_clone.txt"),
      "LICENSE.darts-clone.txt",
    ],
  ];
  for (const [input, output] of licenses) {
    await Bun.write(join(root, output), Bun.file(input));
  }
  await Bun.write(
    join(root, "SOURCE.sd-server.json"),
    `${JSON.stringify(
      {
        version: 1,
        sources: sources.map(({ name, revision, sha256, url }) => ({
          name,
          revision,
          sha256,
          url,
        })),
        patch: {
          name: "inline-wav-audio.patch",
          sha256: await sha256(patchBytes),
        },
      },
      null,
      2,
    )}\n`,
  );
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
