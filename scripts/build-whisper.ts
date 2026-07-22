import { $ } from "bun";
import { chmodSync, mkdirSync, rmSync } from "node:fs";
import { join } from "node:path";

const WHISPER_REPO = "https://github.com/ggml-org/whisper.cpp.git";
const root = join(import.meta.dir, "..");
const tempDir = join(root, "tmp-whisper-build");
const releaseDir = join(root, "release");

function platformSuffix(): string {
  const osName = process.platform;
  const cpuArch = process.arch;
  if (osName === "darwin" && cpuArch === "arm64") return "macos-arm64";
  if (osName === "darwin" && cpuArch === "x64") return "macos-x64";
  if (osName === "linux" && cpuArch === "x64") return "linux-x64";
  if (osName === "linux" && cpuArch === "arm64") return "linux-arm64";
  return `${osName}-${cpuArch}`;
}

async function main() {
  const suffix = platformSuffix();
  const outputName = `whisper-server-${suffix}`;
  const destPath = join(releaseDir, outputName);

  console.log(`\n🚀 Starting local build for whisper-server (${suffix})...`);

  // Clean up any stale temp directories
  rmSync(tempDir, { recursive: true, force: true });
  mkdirSync(tempDir, { recursive: true });
  mkdirSync(releaseDir, { recursive: true });

  console.log(`\n⬇️  Cloning ggml-org/whisper.cpp...`);
  await $`git clone --depth 1 ${WHISPER_REPO} whisper.cpp`.cwd(tempDir);

  const whisperDir = join(tempDir, "whisper.cpp");

  console.log(`\n🛠️  Configuring CMake...`);
  await $`cmake -B build -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DBUILD_SHARED_LIBS=OFF`.cwd(
    whisperDir,
  );

  console.log(`\n🏗️  Compiling whisper-server...`);
  await $`cmake --build build --config Release -j`.cwd(whisperDir);

  // Find the compiled binary
  const buildBinDir = join(whisperDir, "build", "bin");
  const possibleNames = ["whisper-server", "server"];
  let sourcePath = "";

  for (const name of possibleNames) {
    const p = join(buildBinDir, name);
    if (await Bun.file(p).exists()) {
      sourcePath = p;
      break;
    }
    const exePath = join(buildBinDir, `${name}.exe`);
    if (await Bun.file(exePath).exists()) {
      sourcePath = exePath;
      break;
    }
  }

  if (!sourcePath) {
    console.error("❌ Could not locate the compiled whisper-server binary in build/bin.");
    process.exit(1);
  }

  console.log(`\n💾 Copying binary to ${destPath}...`);
  await Bun.write(destPath, Bun.file(sourcePath));
  chmodSync(destPath, (await Bun.file(destPath).stat()).mode | 0o111);

  console.log(`\n🧹 Cleaning up temporary build files...`);
  rmSync(tempDir, { recursive: true, force: true });

  console.log(`\n✅ Success! Compiled binary saved as: ${destPath}`);
}

main().catch((err) => {
  console.error("❌ Build script failed:", err);
  process.exit(1);
});
