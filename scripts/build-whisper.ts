import { $ } from "bun";
import { join } from "node:path";

const WHISPER_SOURCE = {
  revision: "23ee03506a91ac3d3f0071b40e66a430eebdfa1d",
  archiveSha256:
    "c8b0de473e9ec47a74bdf6104425c709261beeada8d6d7c1fec7432be701d032",
};
const archiveUrl = `https://codeload.github.com/ggml-org/whisper.cpp/tar.gz/${WHISPER_SOURCE.revision}`;
const workspace = process.env.GITHUB_WORKSPACE;
const root = workspace ? join(workspace, "build-whisper") : "";
const archivePath = root ? join(root, "whisper.cpp.tar.gz") : "";
const sourcePath = root
  ? join(root, `whisper.cpp-${WHISPER_SOURCE.revision}`)
  : "";
const buildPath = root ? join(root, "build") : "";
const outputPath = root ? join(root, "whisper-server") : "";

function sha256(bytes: Uint8Array): Promise<string> {
  return crypto.subtle
    .digest("SHA-256", bytes)
    .then((digest) =>
      Array.from(new Uint8Array(digest), (byte) =>
        byte.toString(16).padStart(2, "0"),
      ).join(""),
    );
}

async function main() {
  if (process.env.GITHUB_ACTIONS !== "true" || !workspace) {
    throw new Error(
      "Whisper source builds run only in GitHub Actions release jobs.",
    );
  }

  console.log(`Downloading whisper.cpp at ${WHISPER_SOURCE.revision}...`);
  const response = await fetch(archiveUrl);
  if (!response.ok) {
    throw new Error(
      `Failed to download whisper.cpp source: HTTP ${response.status}.`,
    );
  }

  const archive = new Uint8Array(await response.arrayBuffer());
  const actualSha256 = await sha256(archive);
  if (actualSha256 !== WHISPER_SOURCE.archiveSha256) {
    throw new Error(
      `whisper.cpp source checksum mismatch: expected ${WHISPER_SOURCE.archiveSha256}, received ${actualSha256}.`,
    );
  }

  await $`rm -rf ${root}`;
  await $`mkdir -p ${root}`;
  await Bun.write(archivePath, archive);

  console.log("Extracting verified whisper.cpp source...");
  await $`tar --extract --gzip --file ${archivePath} --directory ${root}`;

  console.log("Configuring whisper-server...");
  await $`cmake -S ${sourcePath} -B ${buildPath} -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DBUILD_SHARED_LIBS=OFF`;

  console.log("Building whisper-server...");
  await $`cmake --build ${buildPath} --config Release --target whisper-server --parallel`;

  const binaryPath = join(buildPath, "bin", "whisper-server");
  if (!(await Bun.file(binaryPath).exists())) {
    throw new Error(
      "The Whisper build completed without a whisper-server binary.",
    );
  }

  await Bun.write(outputPath, Bun.file(binaryPath));
  await $`chmod +x ${outputPath}`;
  console.log(`Built verified whisper-server: ${outputPath}`);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
