// Golden video request handling from TS: create-request validation messages,
// native input validation, catalog qualification and the profile-mismatch
// message for every catalog video model, and the job projections (including
// the failure codes and memory messages).
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/video.ts \
//     > go/internal/gateway/testdata/video.json
import { CATALOG } from "../../../src/catalog";
import {
  projectVideoJob,
  qualifiedVideoInput,
  videoCreateRequestSchema,
  videoProfileMismatchMessage,
} from "../../../src/domains/runtime/video/gateway-contract";
import { videoGenerationInputSchema } from "../../../src/domains/runtime/video/video-input";
import {
  testPcm16Wav,
  testPng,
} from "../../../src/domains/runtime/video/video-input.fixtures";
import { RuntimeMemoryAdmissionError } from "../../../src/domains/runtime/memory-controller";
import { ModelInstallConsentError } from "../../../src/domains/runtime/startup-preflight";

function validationMessage(body: unknown): string | null {
  const parsed = videoCreateRequestSchema.safeParse(body);
  if (parsed.success) return null;
  return `Validation failed: ${parsed.error.issues
    .map((issue) => `${issue.path.join(".")}: ${issue.message}`)
    .join(", ")}`;
}

const base = {
  model: "wan2.1-t2v-1.3b-q8_0",
  prompt: "a red fox in the snow",
  width: 320,
  height: 320,
  frames: 33,
  fps: 16,
  input: { kind: "text" },
};
const png = (width: number, height: number, more = {}) =>
  testPng({ width, height, ...more });
const wav = (sampleRate: number, frames: number) =>
  testPcm16Wav({ sampleRate, frames });

const requests: Record<string, unknown> = {
  valid: base,
  "valid-negative": { ...base, negative_prompt: "blurry" },
  "missing-model": { ...base, model: undefined },
  "empty-model": { ...base, model: "" },
  "empty-prompt": { ...base, prompt: "" },
  "long-prompt": { ...base, prompt: "x".repeat(16_385) },
  "string-width": { ...base, width: "320" },
  "fractional-height": { ...base, height: 320.5 },
  "zero-frames": { ...base, frames: 0 },
  "negative-fps": { ...base, fps: -1 },
  "extra-key": { ...base, seconds: 4 },
  "extra-keys": { ...base, seconds: 4, size: "320x320" },
  "missing-input": { ...base, input: undefined },
  "unknown-input": { ...base, input: { kind: "audio" } },
  "input-extra": { ...base, input: { kind: "text", image: "x" } },
  "image-input-missing": { ...base, input: { kind: "image" } },
  "image-input-format": {
    ...base,
    input: { kind: "image", image: { format: "jpeg", data: "abcd" } },
  },
  "reference-with-image": {
    ...base,
    input: { kind: "image", image: { format: "png", data: "abcd" } },
    input_reference: "abcd",
  },
  "reference-too-long": { ...base, input_reference: "x".repeat(6_000_001) },
  "not-object": "video",
  "null-body": null,
  array: [],
};

const inputs: Record<string, unknown> = {
  text: { kind: "text", prompt: "p" },
  "text-full": {
    kind: "text",
    prompt: "p",
    negativePrompt: "n",
    width: 320,
    height: 320,
    videoFrames: 33,
    fps: 16,
    seed: 42,
    outputFormat: "avi",
    generation: { sampler: "euler", scheduler: "discrete", steps: 20, cfgScale: 6, flowShift: 3 },
  },
  "text-bad-sampler": {
    kind: "text",
    prompt: "p",
    generation: { sampler: "dpm", scheduler: "discrete", steps: 20, cfgScale: 6, flowShift: 3 },
  },
  "text-too-many-frames": { kind: "text", prompt: "p", videoFrames: 4_098 },
  "image-ok": {
    kind: "image", prompt: "p", width: 64, height: 32, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(64, 32) },
  },
  "image-rgba-ok": {
    kind: "image", prompt: "p", width: 16, height: 16, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(16, 16, { colorType: 6 }) },
  },
  "image-wrong-size": {
    kind: "image", prompt: "p", width: 64, height: 64, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(64, 32) },
  },
  "image-16-bit": {
    kind: "image", prompt: "p", width: 16, height: 16, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(16, 16, { bitDepth: 16 }) },
  },
  "image-interlaced": {
    kind: "image", prompt: "p", width: 16, height: 16, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(16, 16, { interlace: 1 }) },
  },
  "image-short-scanlines": {
    kind: "image", prompt: "p", width: 16, height: 16, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(16, 16, { inflated: new Uint8Array(10) }) },
  },
  "image-bad-filter": {
    kind: "image", prompt: "p", width: 2, height: 2, videoFrames: 9, fps: 16,
    image: {
      format: "png",
      data: png(2, 2, { inflated: Uint8Array.from([0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0]) }),
    },
  },
  "image-not-canonical": {
    kind: "image", prompt: "p", width: 16, height: 16, videoFrames: 9, fps: 16,
    image: { format: "png", data: png(16, 16).replace(/=+$/, "") + "\n" },
  },
  "image-missing-canvas": {
    kind: "image", prompt: "p", image: { format: "png", data: png(16, 16) },
  },
  "speech-ok": {
    kind: "speech", prompt: "p", width: 32, height: 48, videoFrames: 32, fps: 16,
    portrait: { format: "png", data: png(32, 48) },
    audio: { format: "wav", data: wav(16_000, 32_000) },
  },
  "speech-too-long": {
    kind: "speech", prompt: "p", width: 32, height: 48, videoFrames: 32, fps: 16,
    portrait: { format: "png", data: png(32, 48) },
    audio: { format: "wav", data: wav(16_000, 32_001) },
  },
  "speech-low-rate": {
    kind: "speech", prompt: "p", width: 32, height: 48, videoFrames: 32, fps: 16,
    portrait: { format: "png", data: png(32, 48) },
    audio: { format: "wav", data: wav(7_999, 100) },
  },
  "speech-not-wav": {
    kind: "speech", prompt: "p", width: 32, height: 48, videoFrames: 32, fps: 16,
    portrait: { format: "png", data: png(32, 48) },
    audio: { format: "wav", data: png(4, 4) },
  },
  "speech-wrong-portrait": {
    kind: "speech", prompt: "p", width: 32, height: 32, videoFrames: 32, fps: 16,
    portrait: { format: "png", data: png(32, 48) },
    audio: { format: "wav", data: wav(16_000, 100) },
  },
};

const variants = (spec: (typeof CATALOG)[number]) => {
  const q = spec.videoRuntime!.qualification;
  const fit = {
    model: spec.modelId,
    prompt: "a lighthouse at dusk",
    width: q.maxWidth,
    height: q.maxHeight,
    frames: q.maxFrames,
    fps: q.fps,
    input: { kind: "text" },
  };
  return {
    text: fit,
    "text-negative": { ...fit, negative_prompt: "noise" },
    "wrong-size": { ...fit, width: q.maxWidth + 16 },
    "wrong-frames": { ...fit, frames: q.maxFrames - 1 },
    "wrong-fps": { ...fit, fps: q.fps + 8 },
    image: {
      ...fit,
      input: { kind: "image", image: { format: "png", data: png(q.maxWidth, q.maxHeight) } },
    },
    "image-wrong-size": {
      ...fit,
      input: { kind: "image", image: { format: "png", data: png(16, 16) } },
    },
    reference: { ...fit, input_reference: png(q.maxWidth, q.maxHeight) },
    "reference-data-url": {
      ...fit,
      input_reference: `data:image/png;base64,${png(q.maxWidth, q.maxHeight)}`,
    },
    "reference-wrong-size": { ...fit, input_reference: png(16, 24) },
    speech: {
      ...fit,
      input: {
        kind: "speech",
        portrait: { format: "png", data: png(q.maxWidth, q.maxHeight) },
        audio: { format: "wav", data: wav(16_000, Math.floor((16_000 * q.maxFrames) / q.fps)) },
      },
    },
  };
};

const models = [];
for (const spec of CATALOG) {
  if (spec.kind !== "video" || !spec.videoRuntime) continue;
  const cases = [];
  for (const [name, body] of Object.entries(variants(spec))) {
    const parsed = videoCreateRequestSchema.safeParse(body);
    if (!parsed.success) {
      cases.push({ name, body, schema: validationMessage(body) });
      continue;
    }
    const qualified = qualifiedVideoInput(parsed.data, spec);
    cases.push({
      name,
      body,
      qualified: qualified ?? null,
      mismatch: qualified ? null : videoProfileMismatchMessage(parsed.data, spec),
    });
  }
  models.push({ modelId: spec.modelId, cases });
}

const created = 1_760_000_000_123;
const terminal = 1_760_000_042_999;
const id = "6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab";
const memory = (reason: string, diagnostics?: object, gpu?: object) =>
  new RuntimeMemoryAdmissionError(
    { kind: "rejected", reason: reason as never, poolId: "system" },
    diagnostics as never,
    undefined,
    gpu as never,
  );
const failures: Record<string, Error> = {
  generic: new Error("boom"),
  pressure: memory("memory-pressure"),
  "accelerator-pressure": memory("memory-pressure", undefined, undefined),
  measurement: memory("measurement-unavailable"),
  "system-memory": memory("system-memory", {
    requested_bytes: 12 * 1024 ** 3,
    effective_available_bytes: 15 * 1024 ** 3,
    reserve_bytes: 9.6 * 1024 ** 3,
  }),
  "system-memory-unknown": memory("system-memory", {
    requested_bytes: "unavailable",
    effective_available_bytes: "unavailable",
    reserve_bytes: 9.6 * 1024 ** 3,
  }),
  "accelerator-memory": memory("accelerator-memory", {
    requested_bytes: 30 * 1024 ** 3,
    effective_available_bytes: 20 * 1024 ** 3,
    reserve_bytes: 2 * 1024 ** 3,
  }),
  "gpu-working-set": memory("accelerator-memory", {}, {
    requiredBytes: 20 * 1024 ** 3,
    residentBytes: 30 * 1024 ** 3,
    capBytes: 48 * 1024 ** 3,
  }),
  consent: new ModelInstallConsentError("wan2.1-t2v-1.3b-q8_0"),
};
const projections: Record<string, unknown> = {
  queued: projectVideoJob({ id, state: "queued", createdAtMs: created }),
  in_progress: projectVideoJob({ id, state: "in_progress", createdAtMs: created }),
  completed: projectVideoJob({
    id, state: "completed", createdAtMs: created, terminalAtMs: terminal,
    artifact: { byteLength: 123_456, mimeType: "video/mp4", outputFormat: "mp4", fps: 16, frameCount: 33 },
  }),
};
for (const reason of ["backend", "cancelled", "deadline", "shutdown"] as const) {
  projections[`cancelled-${reason}`] = projectVideoJob({
    id, state: "cancelled", createdAtMs: created, terminalAtMs: terminal, reason,
  });
}
for (const [name, failure] of Object.entries(failures)) {
  projections[`failed-${name}`] = projectVideoJob({
    id, state: "failed", createdAtMs: created, terminalAtMs: terminal, failure,
  });
}

const inputVerdicts: Record<string, boolean> = {};
for (const [name, input] of Object.entries(inputs)) {
  inputVerdicts[name] = videoGenerationInputSchema.safeParse(input).success;
}
const requestMessages: Record<string, unknown> = {};
for (const [name, body] of Object.entries(requests)) {
  requestMessages[name] = { body: body === undefined ? null : body, message: validationMessage(body) };
}

console.log(
  JSON.stringify({ requests: requestMessages, inputs: { cases: inputs, valid: inputVerdicts }, models, projections }, null, 2),
);
