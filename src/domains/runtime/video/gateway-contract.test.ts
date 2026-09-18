import { expect, test } from "bun:test";
import { byId } from "../../../catalog";
import {
  projectVideoJob,
  qualifiedVideoInput,
  type VideoJobResponse,
} from "./gateway-contract";
import { RuntimeMemoryAdmissionError } from "../memory-controller";
import { testPcm16Wav, testPng } from "./video-input.fixtures";

const model = byId("wan2.1-t2v-1.3b-q8_0");
if (!model) throw new Error("Expected qualified Wan video model.");

test("projects only owned memory error types and never failure diagnostics", () => {
  const sentinel = "private prompt, path, or memory diagnostic";
  const memory = new RuntimeMemoryAdmissionError(
    { kind: "rejected", reason: "system-memory", poolId: sentinel },
    {
      measured_available_bytes: 32_035_143_680,
      reserve_bytes: 10_307_921_510,
      pending_bytes: 0,
      requested_bytes: 25_769_803_776,
      effective_available_bytes: 32_035_143_680,
      sample_captured_at_ms: 1,
      sample_age_ms: 0,
      measured_pressure: "normal",
      safety_state: "healthy",
      recovery_samples: 0,
      demand_confidence: "estimated",
    },
  );
  memory.message = sentinel;
  memory.stack = sentinel;
  const lookalike = new Error(sentinel);
  lookalike.name = "RuntimeMemoryAdmissionError";
  for (const [failure, code] of [
    [memory, "insufficient_memory"],
    [lookalike, "video_generation_failed"],
    [new Error("insufficient_memory"), "video_generation_failed"],
  ] satisfies [
    Error,
    Extract<VideoJobResponse, { status: "failed" }>["error"]["code"],
  ][]) {
    const response = projectVideoJob({
      id: "00000000-0000-4000-8000-000000000000",
      state: "failed",
      createdAtMs: 1000,
      terminalAtMs: 2000,
      failure,
    });
    expect(response).toEqual({
      object: "localbase.video.job",
      id: "00000000-0000-4000-8000-000000000000",
      status: "failed",
      created_at: 1,
      completed_at: 2,
      error: { code },
    });
    expect(JSON.stringify(response)).not.toContain(sentinel);
  }
});

test.each([
  {
    id: "wan2.1-t2v-1.3b-q8_0",
    width: 320,
    height: 320,
    frames: 33,
    scheduler: "discrete",
    steps: 20,
    cfgScale: 6,
  },
  {
    id: "fastwan2.2-ti2v-5b-q6_k",
    width: 480,
    height: 832,
    frames: 81,
    scheduler: "lcm",
    steps: 3,
    cfgScale: 1,
  },
])(
  "projects $id into native generation fields",
  ({ id, width, height, frames, scheduler, steps, cfgScale }) => {
    const model = byId(id);
    if (!model) throw new Error("Expected qualified video model.");
    expect(
      qualifiedVideoInput(
        {
          model: model.modelId,
          prompt: "A paper kite over a field.",
          width,
          height,
          frames,
          fps: 16,
          input: { kind: "text" },
        },
        model,
      ),
    ).toEqual({
      kind: "text",
      prompt: "A paper kite over a field.",
      width,
      height,
      videoFrames: frames,
      fps: 16,
      seed: 42,
      outputFormat: "avi",
      generation: {
        sampler: "euler",
        scheduler,
        steps,
        cfgScale,
        flowShift: 3,
      },
    });
  },
);

test("does not construct an unqualified native video request", () => {
  expect(
    qualifiedVideoInput(
      {
        model: model.modelId,
        prompt: "A paper kite over a field.",
        width: 320,
        height: 304,
        frames: 33,
        fps: 16,
        input: { kind: "text" },
      },
      model,
    ),
  ).toBeUndefined();
});

test("projects speech media only for a qualified S2V profile", () => {
  const portrait = testPng({ width: 320, height: 320 });
  const audio = testPcm16Wav({ sampleRate: 8_000, frames: 8_000 });
  const s2vModel = {
    ...model,
    videoRuntime: {
      ...model.videoRuntime!,
      mode: "s2v" as const,
      artifacts: {
        ...model.videoRuntime!.artifacts,
        decoder: {
          kind: "vae" as const,
          artifactFilename: "wan_2.1_vae.safetensors",
        },
        audioEncoder: "audio-encoder.gguf",
      },
    },
  };
  const request = {
    model: s2vModel.modelId,
    prompt: "A speaker reads one sentence.",
    width: 320,
    height: 320,
    frames: 33,
    fps: 16,
    input: {
      kind: "speech" as const,
      portrait: { format: "png" as const, data: portrait },
      audio: { format: "wav" as const, data: audio },
    },
  };

  expect(qualifiedVideoInput(request, s2vModel)).toMatchObject({
    kind: "speech",
    portrait: request.input.portrait,
    audio: request.input.audio,
  });
  expect(
    qualifiedVideoInput(
      { ...request, input: { kind: "text" as const } },
      s2vModel,
    ),
  ).toBeUndefined();
  expect(
    qualifiedVideoInput(
      {
        ...request,
        frames: 15,
        input: {
          ...request.input,
          audio: {
            format: "wav" as const,
            data: testPcm16Wav({ sampleRate: 8_000, frames: 8_000 }),
          },
        },
      },
      {
        ...s2vModel,
        videoRuntime: {
          ...s2vModel.videoRuntime,
          qualification: {
            ...s2vModel.videoRuntime.qualification,
            maxFrames: 15,
          },
        },
      },
    ),
  ).toBeUndefined();
});
