import { constants } from "node:fs";
import { open, stat } from "node:fs/promises";
import { dirname, isAbsolute, join } from "node:path";
import { verifyInventory } from "./inventory";
import { identityModel } from "./smoke-fixture";
import { pcm16Wav } from "./wav";

export type KokoroVoice = "af_heart" | "af_bella";
export type GenerationArguments = {
  kind: "generate";
  model: string;
  prompt: string;
  output: string;
  voice: KokoroVoice;
  inventorySha256: string;
};

export function parseArguments(
  args: string[],
): GenerationArguments | { kind: "smoke"; inventorySha256: string } {
  if (args[0] !== "generate" && args[0] !== "smoke")
    throw new Error("Expected generate or smoke.");
  const options = new Map<string, string>();
  for (let index = 1; index < args.length; index += 2) {
    const key = args[index];
    const value = args[index + 1];
    if (
      !key ||
      ![
        "--model",
        "--prompt",
        "--output",
        "--voice",
        "--inventory-sha256",
      ].includes(key) ||
      !value ||
      value.startsWith("--") ||
      options.has(key)
    )
      throw new Error("Invalid or duplicate CLI option.");
    options.set(key, value);
  }
  const inventorySha256 = options.get("--inventory-sha256");
  if (!inventorySha256 || !/^[a-f0-9]{64}$/.test(inventorySha256))
    throw new Error("A trusted release inventory SHA256 is required.");
  if (args[0] === "smoke") {
    if (options.size !== 1)
      throw new Error("Smoke accepts only the trusted inventory digest.");
    return { kind: "smoke", inventorySha256 };
  }
  const model = options.get("--model");
  const prompt = options.get("--prompt");
  const output = options.get("--output");
  const voice = options.get("--voice") ?? "af_heart";
  if (
    !model ||
    !prompt ||
    !output ||
    ![model, prompt, output].every(isAbsolute) ||
    prompt === output ||
    (voice !== "af_heart" && voice !== "af_bella")
  ) {
    throw new Error(
      "Absolute model/prompt/output paths and an approved voice are required.",
    );
  }
  return { kind: "generate", model, prompt, output, voice, inventorySha256 };
}

async function privatePrompt(path: string): Promise<string> {
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const info = await handle.stat();
    if (!info.isFile() || (info.mode & 0o077) !== 0 || info.size > 1024) {
      throw new Error(
        "Prompt must be a private regular file of at most 1024 bytes.",
      );
    }
    const text = new TextDecoder("utf-8", { fatal: true }).decode(
      await handle.readFile(),
    );
    if (
      Array.from(text).length < 1 ||
      Array.from(text).length > 256 ||
      !text.trim()
    ) {
      throw new Error("Prompt must contain 1 to 256 characters.");
    }
    return text;
  } finally {
    await handle.close();
  }
}

async function main(): Promise<void> {
  const args = parseArguments(Bun.argv.slice(2));
  const root = import.meta.dir;
  for (const key of Object.keys(process.env)) {
    if (/^(NODE_|BUN_OPTIONS$|BUN_PRELOAD$|LD_|DYLD_)/.test(key))
      throw new Error("Unsafe inherited runtime environment.");
  }
  const inventory = await verifyInventory(root, args.inventorySha256);
  const target =
    process.platform === "darwin" && process.arch === "arm64"
      ? "macos-arm64"
      : process.platform === "linux" && process.arch === "x64"
        ? "linux-x64"
        : undefined;
  if (inventory.target !== target)
    throw new Error("Kokoro package target does not match the host.");
  // This executable has no network-dependent operation, including smoke.
  const noNetwork = () => {
    throw new Error("Kokoro runtime network access is disabled.");
  };
  globalThis.fetch = Object.assign(async () => noNetwork(), {
    preconnect: noNetwork,
  });
  const { env } = await import("@huggingface/transformers");
  env.allowLocalModels = true;
  env.allowRemoteModels = false;
  env.useFS = true;
  env.useFSCache = false;
  env.useBrowserCache = false;
  env.useCustomCache = false;
  const { KokoroTTS } = await import("kokoro-js");
  if (args.kind === "smoke") {
    const { InferenceSession, Tensor } = await import("onnxruntime-node");
    const session = await InferenceSession.create(identityModel, {
      executionProviders: ["cpu"],
    });
    try {
      const result = await session.run({
        x: new Tensor("float32", new Float32Array([0, 0.5, -0.5]), [3]),
      });
      const waveform = result.y?.data;
      if (
        !(waveform instanceof Float32Array) ||
        waveform.length !== 3 ||
        waveform[1] !== 0.5 ||
        waveform[2] !== -0.5
      ) {
        throw new Error("Packaged ONNX fixture failed.");
      }
      pcm16Wav(waveform);
      const { phonemize } = await import("phonemizer");
      if (!(await phonemize("Hello", "en-us")).join("").trim())
        throw new Error("Packaged phonemizer failed.");
      for (const voice of ["af_heart", "af_bella"]) {
        if (
          (
            await stat(
              join(root, "node_modules/kokoro-js/voices", `${voice}.bin`),
            )
          ).size !== 522240
        ) {
          throw new Error("Packaged voice is incomplete.");
        }
      }
    } finally {
      await session.release();
    }
    process.stderr.write("Kokoro package smoke completed.\n");
    return;
  }
  const text = await privatePrompt(args.prompt);
  for (const asset of [
    "config.json",
    "tokenizer.json",
    "tokenizer_config.json",
    "onnx/model_quantized.onnx",
  ]) {
    if (!(await stat(join(args.model, asset))).isFile())
      throw new Error(`Required local model asset is missing: ${asset}`);
  }
  const directory = await stat(dirname(args.output));
  if (!directory.isDirectory() || (directory.mode & 0o077) !== 0)
    throw new Error("Output directory must be private.");
  const tts = await KokoroTTS.from_pretrained(args.model, {
    dtype: "q8",
    device: "cpu",
  });
  const audio = await tts.generate(text, { voice: args.voice, speed: 1 });
  if (audio.sampling_rate !== 24000)
    throw new Error("Unexpected speech sample rate.");
  const wav = pcm16Wav(audio.audio);
  const output = await open(args.output, "wx", 0o600);
  try {
    await output.writeFile(wav);
  } finally {
    await output.close();
  }
  process.stderr.write(
    `Kokoro speech completed: ${audio.audio.length} samples.\n`,
  );
}

if (import.meta.main) {
  const timeout = setTimeout(() => {
    process.stderr.write("Kokoro runtime timed out.\n");
    process.exit(1);
  }, 120000);
  main()
    .catch(() => {
      // Dependency errors are not trusted to omit request text.
      process.stderr.write("Kokoro runtime failed.\n");
      process.exitCode = 1;
    })
    .finally(() => clearTimeout(timeout));
}
