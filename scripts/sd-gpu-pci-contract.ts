import assert from "node:assert/strict";

const binaryIndex = Bun.argv.indexOf("--binary");
const binary = binaryIndex === -1 ? undefined : Bun.argv[binaryIndex + 1];
if (!binary) throw new Error("Expected --binary <path>.");

async function run(args: string[]) {
  const child = Bun.spawn([binary!, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: 5_000,
    maxBuffer: 1024 * 1024,
    killSignal: "SIGKILL",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  assert.equal(
    child.signalCode,
    null,
    `sd-server was killed: ${stdout}${stderr}`,
  );
  return { code, output: stdout + stderr };
}

const capability = await run(["--localbase-capabilities"]);
assert.equal(capability.code, 0);
assert.match(capability.output, /^localbase-sd-gpu-pci-v1$/m);

const malformed = await run([
  "--require-gpu-pci",
  "0000:AB:00.0",
  "--diffusion-model",
  "/nonexistent/localbase-model.safetensors",
]);
assert.equal(malformed.code, 1);
assert.match(malformed.output, /canonical lowercase PCI address/);
assert.doesNotMatch(malformed.output, /loading diffusion model/);

const conflict = await run([
  "--require-gpu-pci",
  "ffff:ff:1f.7",
  "--backend",
  "cpu",
  "--diffusion-model",
  "/nonexistent/localbase-model.safetensors",
]);
assert.equal(conflict.code, 1);
assert.match(conflict.output, /conflicts with --backend/);
assert.doesNotMatch(conflict.output, /loading diffusion model/);

const missing = await run([
  "--require-gpu-pci",
  "ffff:ff:1f.7",
  "--diffusion-model",
  "/nonexistent/localbase-model.safetensors",
]);
assert.equal(missing.code, 1);
assert.match(
  missing.output,
  /requires exactly one matching discrete Vulkan GPU/,
);
assert.doesNotMatch(missing.output, /loading diffusion model/);
