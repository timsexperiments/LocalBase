import { $ } from "bun";
import assert from "node:assert/strict";
import { join, resolve } from "node:path";

if (process.env.GITHUB_ACTIONS !== "true" || !process.env.GITHUB_WORKSPACE) {
  throw new Error("Whisper native contract tests run only in GitHub Actions.");
}
const source = process.argv[2];
if (!source)
  throw new Error("Expected the verified, patched Whisper source directory.");
const implementation = await Bun.file(join(source, "src/whisper.cpp")).text();
const start = "static std::vector<ggml_backend_t> whisper_backend_init(";
const end = "using buft_list_t =";
const parts = implementation.split(start);
if (parts.length !== 2 || parts[1]!.split(end).length !== 2) {
  throw new Error("Pinned Whisper backend initialization test anchor changed.");
}
const testDirectory = join(source, "localbase-gpu-tests");
await $`mkdir -p ${testDirectory}`;
await Bun.write(
  join(testDirectory, "whisper-backend-init.inc"),
  start + parts[1]!.split(end)[0],
);
const testSource = resolve(
  import.meta.dir,
  "whisper-patches/require-gpu-pci.test.cpp",
);
const testBinary = join(testDirectory, "require-gpu-pci-test");
await $`c++ -std=c++17 -Wall -Wextra -Werror -I${join(source, "include")} -I${join(source, "ggml/include")} -I${join(source, "examples/server")} -I${testDirectory} ${testSource} -o ${testBinary}`;
await $`${testBinary}`;

const server = process.argv[3];
if (!server) throw new Error("Expected the built Whisper server.");
async function runServer(args: string[], extraEnv: NodeJS.ProcessEnv = {}) {
  const child = Bun.spawn([server!, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: 5_000,
    maxBuffer: 1024 * 1024,
    killSignal: "SIGKILL",
    env: { ...process.env, ...extraEnv },
  });
  try {
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    assert.equal(
      child.signalCode,
      null,
      `Whisper CLI contract test killed: ${stdout}${stderr}`,
    );
    return { code, output: stdout + stderr };
  } finally {
    if (child.exitCode === null && child.signalCode === null)
      child.kill("SIGKILL");
    await child.exited;
  }
}
const capability = await runServer(["--localbase-capabilities"]);
assert.equal(capability.code, 0);
assert.match(capability.output, /^localbase-whisper-gpu-pci-v1$/m);
for (const args of [
  ["--require-gpu-pci"],
  ["--require-gpu-pci", "00000000:01:00.0"],
  ["--require-gpu-pci", "0000:AB:00.0"],
  ["--require-gpu-pci", "0000:01:20.0"],
  ["--require-gpu-pci", "0000:01:00.8"],
  ["--require-gpu-pci", "0000:01:00.0", "--require-gpu-pci", "0000:02:00.0"],
  ["--require-gpu-pci", "0000:01:00.0", "--no-gpu"],
  ["--no-gpu", "--require-gpu-pci", "0000:01:00.0"],
]) {
  const result = await runServer(args);
  assert.equal(result.code, 1);
  assert.match(result.output, /error: --require-gpu-pci/);
}
// The CI runner has no matching physical PCI GPU. An ordinal override must not
// reach model loading, even when both environment and CLI overrides are present.
const missing = await runServer(
  ["--require-gpu-pci", "ffff:ff:1f.7", "--device", "0"],
  { WHISPER_ARG_DEVICE: "1" },
);
assert.equal(missing.code, 1);
assert.match(
  missing.output,
  /requires exactly one matching discrete Vulkan GPU/,
);
assert.doesNotMatch(missing.output, /loading model from/);
