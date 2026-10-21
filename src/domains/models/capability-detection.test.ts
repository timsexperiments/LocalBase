import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CATALOG, type ModelSpec } from "../../catalog";
import type { GgufCapabilityHeader } from "../runtime/gguf-metadata";
import {
  analyzeChatTemplate,
  checkpointCapabilities,
  detectModelCapabilities,
  diffusersCapabilities,
  engineCapabilities,
  ggufCapabilities,
  parseWhisperHeader,
  readSafetensorsTensorNames,
  resolveModelCapabilities,
  whisperCapabilities,
} from "./capability-detection";
import {
  keepFormerCatalogEntries,
  restoreBuiltInCatalog,
} from "../../test/former-catalog";

// Some fixtures are former built-in entries an upgraded install kept.
keepFormerCatalogEntries();
afterAll(restoreBuiltInCatalog);

const root = mkdtempSync(join(tmpdir(), "local-base-capabilities-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

function catalogModel(modelId: string): ModelSpec {
  const model = CATALOG.find((entry) => entry.modelId === modelId);
  if (!model) throw new Error(`Missing catalog fixture ${modelId}`);
  return model;
}

/** Installs placeholder files and sizes the model's artifacts to match them. */
function installed(
  model: ModelSpec,
  files: Readonly<Record<string, Uint8Array | string>> = {},
): { model: ModelSpec; directory: string } {
  const directory = mkdtempSync(join(root, `${model.modelId}-`));
  const artifacts = model.artifacts.map((artifact) => {
    const content = files[artifact.filename] ?? "test";
    const bytes =
      typeof content === "string" ? Buffer.from(content) : Buffer.from(content);
    writeFileSync(join(directory, artifact.filename), bytes);
    return { ...artifact, expectedSizeBytes: bytes.byteLength };
  });
  return { model: { ...model, artifacts }, directory };
}

function header(
  overrides: Partial<GgufCapabilityHeader>,
): GgufCapabilityHeader {
  return {
    architecture: "qwen2",
    contextLength: 32768,
    poolingType: null,
    expertCount: null,
    chatTemplate: null,
    decisionType: null,
    hasVisionEncoder: false,
    hasAudioEncoder: false,
    ...overrides,
  };
}

function whisperBytes(vocab: number, audioLayers: number, textLayers: number) {
  const bytes = Buffer.alloc(64);
  bytes.writeUInt32LE(0x67676d6c, 0);
  bytes.writeInt32LE(vocab, 4);
  bytes.writeInt32LE(audioLayers, 4 + 4 * 4);
  bytes.writeInt32LE(textLayers, 4 + 8 * 4);
  return new Uint8Array(bytes);
}

function safetensors(names: readonly string[]): Uint8Array {
  const json = Buffer.from(
    JSON.stringify({
      __metadata__: { format: "pt" },
      ...Object.fromEntries(
        names.map((name) => [
          name,
          { dtype: "F16", shape: [1], data_offsets: [0, 2] },
        ]),
      ),
    }),
  );
  const length = Buffer.alloc(8);
  length.writeBigUInt64LE(BigInt(json.byteLength));
  return new Uint8Array(Buffer.concat([length, json, Buffer.alloc(2)]));
}

describe("analyzeChatTemplate", () => {
  test("rules tools out when the template never renders them", () => {
    expect(analyzeChatTemplate("{{ messages }}")).toEqual({
      "tool-calling": false,
    });
  });

  test("detects tools and thinking markers", () => {
    expect(
      analyzeChatTemplate(
        "{% if tools %}{{ tools }}{% endif %}{% if enable_thinking %}<think>{% endif %}",
      ),
    ).toEqual({ "tool-calling": true, reasoning: true });
    expect(analyzeChatTemplate("{{ message.tool_calls }}")).toEqual({
      "tool-calling": true,
    });
  });
});

describe("whisper headers", () => {
  test("English-only vocab rules out multilingual and translation", () => {
    const parsed = parseWhisperHeader(whisperBytes(51864, 4, 4));
    expect(parsed).toEqual({ vocabSize: 51864, audioLayers: 4, textLayers: 4 });
    expect(whisperCapabilities(parsed!)).toEqual({
      "speech-to-text": true,
      multilingual: false,
      translation: false,
    });
  });

  test("multilingual checkpoints translate, except large-v3 turbo", () => {
    expect(
      whisperCapabilities(parseWhisperHeader(whisperBytes(51865, 6, 6))!),
    ).toEqual({
      "speech-to-text": true,
      multilingual: true,
      translation: true,
    });
    expect(
      whisperCapabilities(parseWhisperHeader(whisperBytes(51866, 32, 4))!),
    ).toEqual({
      "speech-to-text": true,
      multilingual: true,
      translation: false,
    });
  });

  test("rejects short, foreign, or zeroed headers", () => {
    expect(parseWhisperHeader(new Uint8Array(16))).toBeNull();
    const foreign = whisperBytes(51865, 4, 4);
    foreign[0] = 0;
    expect(parseWhisperHeader(foreign)).toBeNull();
    expect(parseWhisperHeader(whisperBytes(0, 4, 4))).toBeNull();
  });
});

describe("image checkpoints", () => {
  test("reads safetensors tensor names without the metadata entry", async () => {
    const path = join(root, "names.safetensors");
    writeFileSync(path, safetensors(["a.weight", "b.weight"]));
    expect(await readSafetensorsTensorNames(path)).toEqual([
      "a.weight",
      "b.weight",
    ]);
    const bad = join(root, "bad.safetensors");
    writeFileSync(bad, "no");
    expect(await readSafetensorsTensorNames(bad)).toBeNull();
  });

  test("single-file SD checkpoints support image-to-image", () => {
    const sd = [
      "model.diffusion_model.input_blocks.0.0.weight",
      "first_stage_model.decoder.conv_in.weight",
    ];
    expect(checkpointCapabilities(sd)).toEqual({
      capabilities: { "text-to-image": true, "image-to-image": true },
      evidence: ["safetensors: SD UNet checkpoint"],
    });
    expect(
      checkpointCapabilities([...sd, "conditioner.embedders.1.model.x"])
        ?.evidence,
    ).toEqual(["safetensors: SDXL UNet checkpoint"]);
    expect(checkpointCapabilities(["transformer.blocks.0.weight"])).toBeNull();
    expect(checkpointCapabilities([sd[0]!])).toBeNull();
  });

  test("diffusers pipeline classes decide image-to-image", () => {
    expect(
      diffusersCapabilities({ _class_name: "StableDiffusionXLPipeline" })
        ?.capabilities,
    ).toEqual({ "text-to-image": true, "image-to-image": true });
    expect(
      diffusersCapabilities({ _class_name: "FluxPipeline" })?.capabilities,
    ).toEqual({ "text-to-image": true, "image-to-image": false });
    expect(diffusersCapabilities({})).toBeNull();
    expect(diffusersCapabilities(null)).toBeNull();
  });
});

function withProjector(): ModelSpec {
  const base = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");
  return {
    ...base,
    modelId: "vision-fixture",
    artifacts: [
      ...base.artifacts,
      {
        ...base.artifacts[0]!,
        role: "supplementary",
        filename: "mmproj-vision-fixture-f16.gguf",
      },
    ],
  };
}

describe("engineCapabilities", () => {
  test("LLMs with a projector leave input modalities to the files", () => {
    expect(engineCapabilities(withProjector())).toEqual({
      capabilities: {},
      evidence: [],
    });
  });

  test("LLMs without a projector only see text", () => {
    expect(
      engineCapabilities(catalogModel("qwen2.5-coder-7b-instruct-q4_k_m")),
    ).toEqual({ capabilities: {}, inputModalities: ["text"], evidence: [] });
  });

  test("image profiles are text-to-image only", () => {
    expect(
      engineCapabilities(catalogModel("flux1-schnell-q4_0")).capabilities,
    ).toEqual({ "text-to-image": true, "image-to-image": false });
  });

  test("video profiles follow their mode", () => {
    expect(
      engineCapabilities(catalogModel("fastwan2.2-ti2v-5b-q6_k")).capabilities,
    ).toEqual({
      "text-to-video": true,
      "image-to-video": true,
      "speech-to-video": false,
    });
    expect(
      engineCapabilities(catalogModel("wan2.2-s2v-14b-fp8")).capabilities,
    ).toEqual({
      "text-to-video": false,
      "image-to-video": false,
      "speech-to-video": true,
    });
  });
});

describe("ggufCapabilities", () => {
  const chat = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");
  test("reads context, experts, and the chat template", () => {
    expect(
      ggufCapabilities(
        chat,
        header({
          contextLength: 262144,
          expertCount: 64,
          chatTemplate: "{{ tools }}<think>",
        }),
      ),
    ).toEqual({
      capabilities: {
        "long-context": true,
        "mixture-of-experts": true,
        "tool-calling": true,
        reasoning: true,
      },
      evidence: [
        "gguf: qwen2",
        "gguf: context 262144",
        "gguf: 64 experts",
        "gguf: chat template",
      ],
    });
  });

  test("embedders read pooling and ignore copied chat templates", () => {
    const embedder = catalogModel("qwen3-embedding-0.6b-q8_0");
    expect(
      ggufCapabilities(
        embedder,
        header({ poolingType: 3, chatTemplate: "{{ messages }}" }),
      ).capabilities,
    ).toEqual({
      "long-context": false,
      "mixture-of-experts": false,
      embeddings: true,
    });
  });
});

describe("detectModelCapabilities", () => {
  test("models that are not fully installed get engine facts only", async () => {
    const model = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");
    const directory = mkdtempSync(join(root, "empty-"));
    let reads = 0;
    const detected = await detectModelCapabilities(model, directory, {
      ggufHeader: async () => {
        reads += 1;
        return header({});
      },
    });
    expect(reads).toBe(0);
    expect(detected).toEqual({
      capabilities: {},
      inputModalities: ["text"],
      evidence: [],
    });
  });

  test("installed GGUF models use header evidence", async () => {
    const { model, directory } = installed(
      catalogModel("qwen2.5-coder-7b-instruct-q4_k_m"),
    );
    const detected = await detectModelCapabilities(model, directory, {
      ggufHeader: async () => header({ chatTemplate: "{{ messages }}" }),
    });
    expect(detected?.capabilities["tool-calling"]).toBe(false);
    expect(detected?.capabilities["long-context"]).toBe(false);
    expect(detected?.inputModalities).toEqual(["text"]);
  });

  test("installed projectors add their encoders to input modalities", async () => {
    const { model, directory } = installed(withProjector());
    const detected = await detectModelCapabilities(model, directory, {
      ggufHeader: async (path) =>
        path.includes("mmproj")
          ? header({ hasVisionEncoder: true, hasAudioEncoder: true })
          : header({}),
    });
    expect(detected?.inputModalities).toEqual(["text", "image", "audio"]);
    expect(detected?.evidence).toContain("projector: vision encoder");
    expect(detected?.evidence).toContain("projector: audio encoder");
  });

  test("unreadable projectors fall back to text input", async () => {
    const { model, directory } = installed(withProjector());
    const detected = await detectModelCapabilities(model, directory, {
      ggufHeader: async (path) => (path.includes("mmproj") ? null : header({})),
    });
    expect(detected?.inputModalities).toEqual(["text"]);
  });

  test("installed whisper files report vocabulary facts", async () => {
    const base = catalogModel("whisper-base-q8_0");
    const { model, directory } = installed(base, {
      [base.artifacts[0]!.filename]: whisperBytes(51865, 6, 6),
    });
    const detected = await detectModelCapabilities(model, directory);
    expect(detected?.capabilities).toEqual({
      "speech-to-text": true,
      multilingual: true,
      translation: true,
    });
    expect(detected?.evidence).toEqual(["whisper: vocab 51865, 6/6 layers"]);
  });

  test("single-file checkpoints are read; engine false still wins on profiles", async () => {
    const realvis = catalogModel("realvis-xl-v5.0");
    const { model, directory } = installed(realvis, {
      [realvis.artifacts[0]!.filename]: safetensors([
        "model.diffusion_model.input_blocks.0.0.weight",
        "first_stage_model.decoder.conv_in.weight",
        "conditioner.embedders.1.model.ln_final.weight",
      ]),
    });
    const detected = await detectModelCapabilities(model, directory);
    expect(detected?.capabilities).toEqual({
      "text-to-image": true,
      "image-to-image": true,
    });

    const flux = installed(catalogModel("flux1-schnell-q4_0"));
    expect(
      (await detectModelCapabilities(flux.model, flux.directory))?.capabilities[
        "image-to-image"
      ],
    ).toBe(false);
  });
});

describe("resolveModelCapabilities", () => {
  const model = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");

  test("without evidence the declaration stands", () => {
    const resolved = resolveModelCapabilities(model, {});
    expect(resolved.capabilities).toEqual(
      model.capabilities.map(({ id }) => ({ id, source: "declared" })),
    );
    expect(resolved.mismatches).toEqual([]);
    expect(resolved.inputModalities).toEqual({
      values: model.inputModalities,
      source: "declared",
    });
  });

  test("runtime beats detected beats declared, keeping disagreements", () => {
    const resolved = resolveModelCapabilities(model, {
      detected: {
        capabilities: { "tool-calling": true, "long-context": true },
        inputModalities: ["text"],
      },
      runtime: { capabilities: { "tool-calling": false } },
    });
    expect(resolved.capabilities.map(({ id }) => id)).not.toContain(
      "tool-calling",
    );
    expect(resolved.capabilities).toContainEqual({
      id: "long-context",
      source: "detected",
    });
    expect(resolved.mismatches).toEqual([
      {
        id: "tool-calling",
        declared: true,
        detected: true,
        runtime: false,
        effective: false,
      },
      { id: "long-context", declared: false, detected: true, effective: true },
    ]);
    expect(resolved.inputModalities).toEqual({
      values: ["text"],
      source: "detected",
    });
  });

  test("declared capabilities keep catalog order; new ones follow", () => {
    const resolved = resolveModelCapabilities(model, {
      detected: { capabilities: { reasoning: true } },
      runtime: { capabilities: {}, inputModalities: ["text", "image"] },
    });
    const ids = resolved.capabilities.map(({ id }) => id);
    expect(ids).toEqual([
      ...model.capabilities.map(({ id }) => id),
      "reasoning",
    ]);
    expect(resolved.inputModalities.source).toBe("runtime");
  });
});

describe("ggufCapabilities for decision models", () => {
  const chat = catalogModel("qwen2.5-coder-7b-instruct-q4_k_m");
  test("a decision type detects decisions and skips the template", () => {
    const result = ggufCapabilities(
      chat,
      header({
        architecture: "clef",
        decisionType: "clef",
        chatTemplate: "{{ tools }}<think>",
      }),
    );
    expect(result.capabilities.decision).toBe(true);
    expect(result.capabilities["tool-calling"]).toBeUndefined();
    expect(result.capabilities.reasoning).toBeUndefined();
    expect(result.evidence).toContain("gguf: decision type clef");
  });

  test("a declared decision model without decision heads is refuted", () => {
    const declared: ModelSpec = {
      ...chat,
      llmRuntime: { capability: "decision-only" },
    };
    expect(
      ggufCapabilities(declared, header({ chatTemplate: "{{ tools }}" }))
        .capabilities.decision,
    ).toBe(false);
  });
});
