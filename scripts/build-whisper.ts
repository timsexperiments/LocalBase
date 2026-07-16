import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, copyFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { platform, arch } from "node:os";

const WHISPER_REPO = "https://github.com/ggml-org/whisper.cpp.git";
const root = join(__dirname, "..");
const tempDir = join(root, "tmp-whisper-build");
const releaseDir = join(root, "release");

function platformSuffix(): string {
  const osName = platform();
  const cpuArch = arch();
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
  if (existsSync(tempDir)) {
    rmSync(tempDir, { recursive: true, force: true });
  }
  mkdirSync(tempDir, { recursive: true });
  mkdirSync(releaseDir, { recursive: true });

  console.log(`\n⬇️  Cloning ggml-org/whisper.cpp...`);
  const clone = spawnSync("git", ["clone", "--depth", "1", WHISPER_REPO, "whisper.cpp"], {
    cwd: tempDir,
    stdio: "inherit"
  });
  if (clone.status !== 0) {
    console.error("❌ Failed to clone whisper.cpp repo.");
    process.exit(1);
  }

  const whisperDir = join(tempDir, "whisper.cpp");

  console.log(`\n🛠️  Configuring CMake...`);
  const config = spawnSync("cmake", ["-B", "build", "-DWHISPER_BUILD_TESTS=OFF", "-DWHISPER_BUILD_EXAMPLES=ON", "-DBUILD_SHARED_LIBS=OFF"], {
    cwd: whisperDir,
    stdio: "inherit"
  });
  if (config.status !== 0) {
    console.error("❌ Failed to configure CMake.");
    process.exit(1);
  }

  console.log(`\n🏗️  Compiling whisper-server...`);
  const build = spawnSync("cmake", ["--build", "build", "--config", "Release", "-j"], {
    cwd: whisperDir,
    stdio: "inherit"
  });
  if (build.status !== 0) {
    console.error("❌ Failed to compile whisper-server.");
    process.exit(1);
  }

  // Find the compiled binary
  const buildBinDir = join(whisperDir, "build", "bin");
  const possibleNames = ["whisper-server", "server"];
  let sourcePath = "";

  for (const name of possibleNames) {
    const p = join(buildBinDir, name);
    if (existsSync(p)) {
      sourcePath = p;
      break;
    }
    const exePath = join(buildBinDir, `${name}.exe`);
    if (existsSync(exePath)) {
      sourcePath = exePath;
      break;
    }
  }

  if (!sourcePath) {
    console.error("❌ Could not locate the compiled whisper-server binary in build/bin.");
    process.exit(1);
  }

  console.log(`\n💾 Copying binary to ${destPath}...`);
  copyFileSync(sourcePath, destPath);
  spawnSync("chmod", ["+x", destPath]);

  console.log(`\n🧹 Cleaning up temporary build files...`);
  rmSync(tempDir, { recursive: true, force: true });

  console.log(`\n✅ Success! Compiled binary saved as: ${destPath}`);
}

main().catch((err) => {
  console.error("❌ Build script failed:", err);
  process.exit(1);
});
