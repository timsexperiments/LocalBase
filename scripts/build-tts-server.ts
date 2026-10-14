import { $ } from "bun";
import { availableParallelism } from "node:os";
import { join } from "node:path";

export const LLAMA_CPP_TAG = "b10419";
export const LLAMA_CPP_REVISION = "aee56b3abfeb134218fe5c496c329a1faac1ef28";
export const LLAMA_CPP_SHA256 =
  "43ed9540769b26a2f180e60d0f14ffd2eaef7f844ab12b40f5b7b6fa37a2c387";
const LLAMA_CPP_URL = `https://codeload.github.com/ggml-org/llama.cpp/tar.gz/${LLAMA_CPP_REVISION}`;

/** LocalBase sources compiled into the llama.cpp tree as `tools/tts-server`. */
export const ttsServerSourceFiles = [
  "CMakeLists.txt",
  "tts-server.cpp",
] as const;

async function sha256(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

/**
 * Registers tts-server beside upstream llama-tts. Exactly one anchor must exist
 * so an upstream layout change fails the build instead of silently skipping it.
 */
export function registerTtsServerTool(cmakeLists: string): string {
  const anchor = "    add_subdirectory(tts)\n";
  if (cmakeLists.split(anchor).length !== 2) {
    throw new Error(
      "llama.cpp tools/CMakeLists.txt must register the tts tool exactly once.",
    );
  }
  if (cmakeLists.includes("add_subdirectory(tts-server)")) {
    throw new Error("llama.cpp already registers a tts-server tool.");
  }
  return cmakeLists.replace(
    anchor,
    `${anchor}    add_subdirectory(tts-server)\n`,
  );
}

async function main() {
  const workspace = process.env.GITHUB_WORKSPACE;
  if (process.env.GITHUB_ACTIONS !== "true" || !workspace) {
    throw new Error("tts-server source builds run only in GitHub Actions.");
  }

  const root = join(workspace, "build-tts-server");
  const extractionPath = join(root, "extract");
  const sourcePath = join(extractionPath, `llama.cpp-${LLAMA_CPP_REVISION}`);
  const buildPath = join(root, "build");
  await $`rm -rf ${root}`;
  await $`mkdir -p ${extractionPath}`;

  const response = await fetch(LLAMA_CPP_URL);
  if (!response.ok) {
    throw new Error(`Failed to download llama.cpp: HTTP ${response.status}.`);
  }
  const archive = new Uint8Array(await response.arrayBuffer());
  const digest = await sha256(archive);
  if (digest !== LLAMA_CPP_SHA256) {
    throw new Error(
      `llama.cpp source checksum mismatch: expected ${LLAMA_CPP_SHA256}, received ${digest}.`,
    );
  }
  const archivePath = join(root, "llama.cpp.tar.gz");
  await Bun.write(archivePath, archive);
  await $`tar --extract --gzip --file ${archivePath} --directory ${extractionPath}`;

  const toolsCmake = join(sourcePath, "tools/CMakeLists.txt");
  await Bun.write(
    toolsCmake,
    registerTtsServerTool(await Bun.file(toolsCmake).text()),
  );
  const localSources = await Promise.all(
    ttsServerSourceFiles.map(async (name) => {
      const path = join(workspace, "scripts/tts-server", name);
      const bytes = new Uint8Array(await Bun.file(path).arrayBuffer());
      await Bun.write(join(sourcePath, "tools/tts-server", name), bytes);
      return { name, sha256: await sha256(bytes) };
    }),
  );

  const platformFlags =
    process.platform === "linux"
      ? ["-DGGML_VULKAN=ON", "-DGGML_OPENMP=OFF"]
      : ["-DGGML_METAL=ON", "-DCMAKE_OSX_DEPLOYMENT_TARGET=14.0"];
  await $`cmake -S ${sourcePath} -B ${buildPath} -DCMAKE_BUILD_TYPE=Release -DGGML_NATIVE=OFF -DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF -DLLAMA_OPENSSL=OFF ${platformFlags}`;
  // A bare --parallel gives Make an unlimited -j, which exhausts runner memory.
  await $`cmake --build ${buildPath} --config Release --target tts-server --parallel ${availableParallelism()}`;

  const binary = join(buildPath, "bin", "tts-server");
  if (!(await Bun.file(binary).exists())) {
    throw new Error("The native build completed without tts-server.");
  }
  await Bun.write(join(root, "tts-server"), Bun.file(binary));
  await $`chmod +x ${join(root, "tts-server")}`;

  const licenses: Array<[string, string]> = [
    [join(sourcePath, "LICENSE"), "LICENSE.llama.cpp.txt"],
    [join(sourcePath, "vendor/cpp-httplib/LICENSE"), "LICENSE.cpp-httplib.txt"],
    [join(sourcePath, "licenses/LICENSE-jsonhpp"), "LICENSE.nlohmann-json.txt"],
  ];
  for (const [input, output] of licenses) {
    await Bun.write(join(root, output), Bun.file(input));
  }
  await Bun.write(
    join(root, "SOURCE.tts-server.json"),
    `${JSON.stringify(
      {
        version: 1,
        sources: [
          {
            name: "llama.cpp",
            tag: LLAMA_CPP_TAG,
            revision: LLAMA_CPP_REVISION,
            sha256: LLAMA_CPP_SHA256,
            url: LLAMA_CPP_URL,
          },
        ],
        localSources,
        runtimeRequirements: {
          "linux-x64": {
            buildBaseline: "Ubuntu 24.04 x86_64",
            gpu: "Vulkan loader and a compatible Vulkan GPU driver",
            systemLibraries:
              "glibc and libstdc++ compatible with the Ubuntu 24.04 build baseline",
          },
          "macos-arm64": {
            buildBaseline: "macOS 14 arm64",
            gpu: "Apple Metal",
            systemLibraries: "macOS system frameworks",
          },
        },
      },
      null,
      2,
    )}\n`,
  );
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(error);
    process.exit(1);
  });
}
