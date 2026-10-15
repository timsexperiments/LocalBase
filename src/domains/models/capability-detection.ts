import { join } from "node:path";
import {
  llmProjectorArtifact,
  primaryArtifact,
  resolveCatalogInstallation,
  type ModelModality,
  type ModelSpec,
} from "../../catalog";
import {
  hasCapability,
  type Capability,
  type CapabilityMismatch,
  type CapabilitySource,
  type ModelCapability,
} from "../../catalog/capabilities";
import { createGgufCapabilityHeaderReader } from "../runtime/gguf-geometry-cache";
import type { GgufCapabilityHeader } from "../runtime/gguf-metadata";

/**
 * A source's opinion per capability: true supports it, false rules it out,
 * and a missing key means the source cannot tell.
 */
export type CapabilityOpinions = Readonly<Partial<Record<Capability, boolean>>>;

export interface CapabilityEvidence {
  readonly capabilities: CapabilityOpinions;
  /** Input modalities this source vouches for; absent when it cannot tell. */
  readonly inputModalities?: readonly ModelModality[];
  /**
   * Opinions from a real round-trip that exercised the capability (for
   * example a tool-call probe). Template flags never populate this; only
   * these opinions override a verified-unsupported catalog entry.
   */
  readonly verified?: CapabilityOpinions;
}

export interface DetectedCapabilities extends CapabilityEvidence {
  /** Short human-readable facts the opinions came from. */
  readonly evidence: readonly string[];
}

export type { CapabilityMismatch };

export interface ResolvedCapabilities {
  /** Effective capabilities with the source that decided each one. */
  readonly capabilities: readonly ModelCapability[];
  readonly mismatches: readonly CapabilityMismatch[];
  readonly inputModalities: Readonly<{
    values: readonly ModelModality[];
    source: CapabilitySource;
  }>;
}

// ---------------------------------------------------------------------------
// Chat templates

const TOOL_MARKERS = /\btools\b|\btool_calls\b/;
const REASONING_MARKERS =
  /<think>|\benable_thinking\b|\breasoning_content\b|\breasoning_effort\b|\bthinking\b/;

/**
 * Reads tool and reasoning support from a Jinja chat template. A template
 * that never renders tools cannot pass them to the model, so a missing tool
 * marker rules tool calling out. Reasoning is only ever asserted: plenty of
 * models reason without the template mentioning it.
 */
export function analyzeChatTemplate(template: string): CapabilityOpinions {
  return {
    "tool-calling": TOOL_MARKERS.test(template),
    ...(REASONING_MARKERS.test(template) ? { reasoning: true } : {}),
  };
}

// ---------------------------------------------------------------------------
// whisper.cpp ggml headers

const WHISPER_MAGIC = 0x67676d6c;
const WHISPER_ENGLISH_VOCAB = 51864;

export interface WhisperHeader {
  readonly vocabSize: number;
  readonly audioLayers: number;
  readonly textLayers: number;
}

/** Parses the fixed hyperparameter block at the start of a whisper ggml file. */
export function parseWhisperHeader(bytes: Uint8Array): WhisperHeader | null {
  if (bytes.byteLength < 48) return null;
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (view.getUint32(0, true) !== WHISPER_MAGIC) return null;
  const field = (index: number) => view.getInt32(4 + index * 4, true);
  const header = {
    vocabSize: field(0),
    audioLayers: field(4),
    textLayers: field(8),
  };
  return header.vocabSize > 0 && header.audioLayers > 0 && header.textLayers > 0
    ? header
    : null;
}

/**
 * Multilingual whisper checkpoints carry the larger vocabulary. large-v3
 * turbo (32 encoder layers, 4 decoder layers) was not trained to translate.
 */
export function whisperCapabilities(header: WhisperHeader): CapabilityOpinions {
  const multilingual = header.vocabSize > WHISPER_ENGLISH_VOCAB;
  const turbo = header.audioLayers === 32 && header.textLayers === 4;
  return {
    "speech-to-text": true,
    multilingual,
    translation: multilingual && !turbo,
  };
}

async function readWhisperHeader(path: string): Promise<WhisperHeader | null> {
  try {
    const bytes = new Uint8Array(
      await Bun.file(path).slice(0, 48).arrayBuffer(),
    );
    return parseWhisperHeader(bytes);
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Image checkpoints

const MAX_SAFETENSORS_HEADER_BYTES = 32 * 1024 * 1024;

/** Reads the tensor names from a safetensors header without loading weights. */
export async function readSafetensorsTensorNames(
  path: string,
): Promise<readonly string[] | null> {
  try {
    const file = Bun.file(path);
    const prefix = new DataView(await file.slice(0, 8).arrayBuffer());
    if (prefix.byteLength < 8) return null;
    const length = Number(prefix.getBigUint64(0, true));
    if (!Number.isSafeInteger(length) || length <= 0) return null;
    if (length > MAX_SAFETENSORS_HEADER_BYTES) return null;
    const header: unknown = JSON.parse(await file.slice(8, 8 + length).text());
    if (!header || typeof header !== "object" || Array.isArray(header))
      return null;
    return Object.keys(header).filter((key) => key !== "__metadata__");
  } catch {
    return null;
  }
}

/**
 * A single-file Stable Diffusion family checkpoint (UNet with its own VAE
 * and text encoder) runs through stable-diffusion.cpp's `-m` path, which
 * accepts an init image for image-to-image.
 */
export function checkpointCapabilities(
  tensorNames: readonly string[],
): DetectedCapabilities | null {
  const unet = tensorNames.some((name) =>
    name.startsWith("model.diffusion_model.input_blocks."),
  );
  if (!unet) return null;
  const vae = tensorNames.some((name) => name.startsWith("first_stage_model."));
  if (!vae) return null;
  const sdxl = tensorNames.some((name) =>
    name.startsWith("conditioner.embedders.1."),
  );
  return {
    capabilities: { "text-to-image": true, "image-to-image": true },
    evidence: [`safetensors: ${sdxl ? "SDXL" : "SD"} UNet checkpoint`],
  };
}

const IMAGE_TO_IMAGE_PIPELINE =
  /^StableDiffusion(XL)?(Img2Img|Inpaint)?Pipeline$|Img2Img|Inpaint/;

/** Reads a local diffusers `model_index.json` pipeline class. */
export function diffusersCapabilities(
  modelIndex: unknown,
): DetectedCapabilities | null {
  if (!modelIndex || typeof modelIndex !== "object") return null;
  const className = (modelIndex as Record<string, unknown>)._class_name;
  if (typeof className !== "string" || !className) return null;
  return {
    capabilities: {
      "text-to-image": true,
      "image-to-image": IMAGE_TO_IMAGE_PIPELINE.test(className),
    },
    evidence: [`model_index.json: ${className}`],
  };
}

// ---------------------------------------------------------------------------
// Engine facts

/**
 * What the pinned engines and LocalBase's launch profiles allow regardless of
 * the files. These intersect with file detection: an engine "false" wins over
 * a file "true" because the request cannot be served.
 */
export function engineCapabilities(model: ModelSpec): DetectedCapabilities {
  if (model.kind === "llm") {
    // The launcher passes --mmproj only when the catalog ships a projector, so
    // models without one only see text. With one, the projector file decides.
    if (llmProjectorArtifact(model)) return { capabilities: {}, evidence: [] };
    return { capabilities: {}, inputModalities: ["text"], evidence: [] };
  }
  if (model.kind === "image" && model.imageRuntime) {
    return {
      capabilities: { "text-to-image": true, "image-to-image": false },
      evidence: [
        `image profile ${model.imageRuntime.kind}: text-to-image only`,
      ],
    };
  }
  if (model.kind === "video" && model.videoRuntime) {
    const mode = model.videoRuntime.mode;
    return {
      capabilities: {
        "text-to-video": mode === "t2v" || mode === "ti2v",
        "image-to-video": mode === "ti2v",
        "speech-to-video": mode === "s2v",
      },
      evidence: [`video profile ${mode}`],
    };
  }
  return { capabilities: {}, evidence: [] };
}

// ---------------------------------------------------------------------------
// GGUF language models

const LONG_CONTEXT_TOKENS = 128 * 1024;

export function ggufCapabilities(
  model: ModelSpec,
  header: GgufCapabilityHeader,
): DetectedCapabilities {
  const capabilities: Partial<Record<Capability, boolean>> = {};
  const evidence: string[] = [];
  if (header.architecture) evidence.push(`gguf: ${header.architecture}`);
  if (header.contextLength !== null) {
    capabilities["long-context"] = header.contextLength >= LONG_CONTEXT_TOKENS;
    evidence.push(`gguf: context ${header.contextLength}`);
  }
  capabilities["mixture-of-experts"] = header.expertCount !== null;
  if (header.expertCount !== null)
    evidence.push(`gguf: ${header.expertCount} experts`);

  if (model.llmRuntime?.capability === "embedding-only") {
    // Embedding templates often copy the chat template; they never chat.
    if (header.poolingType !== null) {
      capabilities.embeddings = true;
      evidence.push(`gguf: pooling type ${header.poolingType}`);
    }
  } else if (header.chatTemplate) {
    Object.assign(capabilities, analyzeChatTemplate(header.chatTemplate));
    evidence.push("gguf: chat template");
  }
  return { capabilities, evidence };
}

// ---------------------------------------------------------------------------
// Detection entry point

export type CapabilityDetectionReaders = Readonly<{
  ggufHeader: (path: string) => Promise<GgufCapabilityHeader | null>;
}>;

const defaultReaders: CapabilityDetectionReaders = {
  ggufHeader: createGgufCapabilityHeaderReader(),
};

const fileCache = new Map<
  string,
  { signature: string; value: Promise<DetectedCapabilities | null> }
>();

async function fileSignature(path: string): Promise<string | null> {
  const stat = await Bun.file(path)
    .stat()
    .catch(() => null);
  return stat ? `${stat.size}:${stat.mtimeMs}` : null;
}

/** Caches non-GGUF file detection while the inspected file is unchanged. */
async function cachedFileDetection(
  key: string,
  path: string,
  detect: () => Promise<DetectedCapabilities | null>,
): Promise<DetectedCapabilities | null> {
  const signature = await fileSignature(path);
  if (!signature) return null;
  let entry = fileCache.get(key);
  if (!entry || entry.signature !== signature) {
    entry = { signature, value: detect() };
    fileCache.set(key, entry);
  }
  const value = await entry.value;
  if (value === null && fileCache.get(key) === entry) fileCache.delete(key);
  return value;
}

async function detectFromFiles(
  model: ModelSpec,
  kindDirectory: string,
  readers: CapabilityDetectionReaders,
): Promise<DetectedCapabilities | null> {
  const primaryPath = join(kindDirectory, primaryArtifact(model).filename);
  switch (model.kind) {
    case "llm": {
      const header = await readers.ggufHeader(primaryPath);
      if (!header) return null;
      const detected = ggufCapabilities(model, header);
      const projectorArtifact = llmProjectorArtifact(model);
      const projector = projectorArtifact
        ? await readers.ggufHeader(
            join(kindDirectory, projectorArtifact.filename),
          )
        : null;
      const evidence = [...detected.evidence];
      const inputModalities: ModelModality[] = ["text"];
      if (projector?.hasVisionEncoder) {
        inputModalities.push("image");
        evidence.push("projector: vision encoder");
      }
      if (projector?.hasAudioEncoder) {
        inputModalities.push("audio");
        evidence.push("projector: audio encoder");
      }
      return { ...detected, inputModalities, evidence };
    }
    case "stt":
      return cachedFileDetection(
        `stt:${primaryPath}`,
        primaryPath,
        async () => {
          const header = await readWhisperHeader(primaryPath);
          if (!header) return null;
          return {
            capabilities: whisperCapabilities(header),
            evidence: [
              `whisper: vocab ${header.vocabSize}, ${header.audioLayers}/${header.textLayers} layers`,
            ],
          };
        },
      );
    case "image": {
      const index = model.artifacts.find(({ filename }) =>
        filename.endsWith("model_index.json"),
      );
      if (index) {
        const path = join(kindDirectory, index.filename);
        return cachedFileDetection(`image:${path}`, path, async () =>
          diffusersCapabilities(
            await Bun.file(path)
              .json()
              .catch(() => null),
          ),
        );
      }
      if (model.imageRuntime || !primaryPath.endsWith(".safetensors"))
        return null;
      return cachedFileDetection(
        `image:${primaryPath}`,
        primaryPath,
        async () => {
          const names = await readSafetensorsTensorNames(primaryPath);
          return names ? checkpointCapabilities(names) : null;
        },
      );
    }
    default:
      return null;
  }
}

/**
 * Detects capabilities from the installed files intersected with engine
 * facts. Models that are not completely installed get engine facts only, so
 * their declared capabilities stay the display fallback.
 */
export async function detectModelCapabilities(
  model: ModelSpec,
  kindDirectory: string,
  readers: CapabilityDetectionReaders = defaultReaders,
): Promise<DetectedCapabilities | null> {
  const engine = engineCapabilities(model);
  const installation = await resolveCatalogInstallation(model, kindDirectory);
  const files = installation.complete
    ? await detectFromFiles(model, kindDirectory, readers)
    : null;
  // Engine "true" fills gaps, file evidence overrides it, engine "false" wins.
  const capabilities = {
    ...opinionsEqualTo(engine.capabilities, true),
    ...files?.capabilities,
    ...opinionsEqualTo(engine.capabilities, false),
  };
  const inputModalities = engine.inputModalities ?? files?.inputModalities;
  const evidence = [...(files?.evidence ?? []), ...engine.evidence];
  if (
    Object.keys(capabilities).length === 0 &&
    !inputModalities &&
    evidence.length === 0
  )
    return null;
  return {
    capabilities,
    ...(inputModalities ? { inputModalities } : {}),
    evidence,
  };
}

function opinionsEqualTo(
  opinions: CapabilityOpinions,
  wanted: boolean,
): CapabilityOpinions {
  return Object.fromEntries(
    Object.entries(opinions).filter(([, value]) => value === wanted),
  );
}

// ---------------------------------------------------------------------------
// Resolution

/**
 * Resolves the effective capability set. Each capability takes the highest
 * source with an opinion: runtime over detected over the catalog
 * declaration. Disagreements are kept as mismatches for diagnostics.
 */
export function resolveModelCapabilities(
  model: ModelSpec,
  sources: Readonly<{
    detected?: CapabilityEvidence | null;
    runtime?: CapabilityEvidence | null;
  }>,
): ResolvedCapabilities {
  const detected = sources.detected?.capabilities ?? {};
  const runtime = sources.runtime?.capabilities ?? {};
  const verified = sources.runtime?.verified ?? {};
  const unsupported = new Map(
    (model.unsupportedCapabilities ?? []).map((entry) => [entry.id, entry]),
  );
  const ids = new Set<Capability>([
    ...model.capabilities.map(({ id }) => id),
    ...unsupported.keys(),
    ...(Object.keys(detected) as Capability[]),
    ...(Object.keys(runtime) as Capability[]),
  ]);
  const capabilities: ModelCapability[] = [];
  const mismatches: CapabilityMismatch[] = [];
  for (const id of ids) {
    const declared = hasCapability(model, id);
    const detectedValue = detected[id];
    const runtimeValue = runtime[id];
    const verifiedValue = verified[id];
    const unsupportedEntry = unsupported.get(id);
    if (unsupportedEntry) {
      // Verified unsupported: template detection and engine flags cannot
      // turn it back on; only a verified round-trip can.
      const effective = verifiedValue === true;
      if (effective) capabilities.push({ id, source: "runtime" });
      if (effective || detectedValue || runtimeValue) {
        mismatches.push({
          id,
          declared: false,
          ...(detectedValue !== undefined ? { detected: detectedValue } : {}),
          ...(runtimeValue !== undefined ? { runtime: runtimeValue } : {}),
          ...(verifiedValue !== undefined ? { verified: verifiedValue } : {}),
          effective,
          unsupportedReason: unsupportedEntry.reason,
        });
      }
      continue;
    }
    const [effective, source]: [boolean, CapabilitySource] =
      verifiedValue !== undefined
        ? [verifiedValue, "runtime"]
        : runtimeValue !== undefined
          ? [runtimeValue, "runtime"]
          : detectedValue !== undefined
            ? [detectedValue, "detected"]
            : [declared, "declared"];
    if (effective) capabilities.push({ id, source });
    const opinions = [
      declared,
      detectedValue,
      runtimeValue,
      verifiedValue,
    ].filter((value): value is boolean => value !== undefined);
    if (opinions.some((value) => value !== opinions[0])) {
      mismatches.push({
        id,
        declared,
        ...(detectedValue !== undefined ? { detected: detectedValue } : {}),
        ...(runtimeValue !== undefined ? { runtime: runtimeValue } : {}),
        ...(verifiedValue !== undefined ? { verified: verifiedValue } : {}),
        effective,
      });
    }
  }
  const order = new Map(model.capabilities.map(({ id }, index) => [id, index]));
  capabilities.sort(
    (a, b) =>
      (order.get(a.id) ?? Number.MAX_SAFE_INTEGER) -
      (order.get(b.id) ?? Number.MAX_SAFE_INTEGER),
  );

  const inputModalities = sources.runtime?.inputModalities
    ? { values: sources.runtime.inputModalities, source: "runtime" as const }
    : sources.detected?.inputModalities
      ? {
          values: sources.detected.inputModalities,
          source: "detected" as const,
        }
      : { values: model.inputModalities, source: "declared" as const };
  return { capabilities, mismatches, inputModalities };
}
