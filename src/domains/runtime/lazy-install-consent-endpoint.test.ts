import { expect, test } from "bun:test";
import { rmSync } from "node:fs";
import { join } from "node:path";
import { byId, CATALOG, type ModelSpec } from "../../catalog";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
  TTS_MODEL,
} from "../../test/gateway-fixture";
import { minimalWav } from "../../test/media-fixtures";

registerGatewayFixtureCleanup();

function anotherModel(kind: ModelSpec["kind"], excluded: string): ModelSpec {
  const model = CATALOG.find(
    (candidate) => candidate.kind === kind && candidate.modelId !== excluded,
  );
  return model ?? CATALOG.find((candidate) => candidate.kind === kind)!;
}

async function assertConsentError(response: Response): Promise<void> {
  expect(response.status).toBe(409);
  expect(await response.json()).toMatchObject({
    error: {
      type: "invalid_request_error",
      param: "model",
      code: "model_install_consent_required",
      message: expect.stringContaining("--install-missing"),
    },
  });
}

test("lazy HTTP model installs require consent for every downloadable gateway modality", async () => {
  const gateway = await startGatewayFixture({ ttsEnabled: true });
  try {
    const llm = anotherModel("llm", "qwen2.5-coder-1.5b-instruct-q4_k_m");
    const stt = anotherModel("stt", "whisper-large-v3-turbo");
    const tts = anotherModel("tts", TTS_MODEL);
    const image = anotherModel("image", "stable-diffusion-v1-5");
    const transcription = new FormData();
    transcription.set("model", stt.modelId);
    transcription.set(
      "file",
      new File([minimalWav], "fixture.wav", { type: "audio/wav" }),
    );
    const requests: Array<{
      kind: ModelSpec["kind"];
      model: ModelSpec;
      directory: string;
      path: string;
      init: RequestInit;
    }> = [
      {
        kind: "llm",
        model: llm,
        directory: "llm",
        path: "/v1/chat/completions",
        init: {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            model: llm.modelId,
            messages: [{ role: "user", content: "hello" }],
          }),
        },
      },
      {
        kind: "stt",
        model: stt,
        directory: "stt",
        path: "/v1/audio/transcriptions",
        init: { method: "POST", body: transcription },
      },
      {
        kind: "tts",
        model: tts,
        directory: "tts",
        path: "/v1/audio/speech",
        init: {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            model: tts.modelId,
            input: "hello",
            voice: "default",
            response_format: "wav",
          }),
        },
      },
      {
        kind: "image",
        model: image,
        directory: "image",
        path: "/v1/images/generations",
        init: {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            model: image.modelId,
            prompt: "a small red house",
          }),
        },
      },
    ];

    for (const request of requests) {
      const spec = byId(request.model.modelId);
      if (!spec) throw new Error(`Missing model ${request.model.modelId}.`);
      for (const artifact of spec.artifacts) {
        rmSync(
          join(gateway.root, "models", request.directory, artifact.filename),
          { force: true },
        );
      }
      const config = gateway.readConfig();
      if (request.kind === "llm") {
        config.activeLlmModel = request.model.modelId;
        config.selectedLlmModels = [request.model.modelId];
      }
      if (request.kind === "stt") {
        config.activeSttModel = request.model.modelId;
        config.selectedSttModels = [request.model.modelId];
      }
      if (request.kind === "tts") {
        config.activeTtsModel = request.model.modelId;
        config.selectedTtsModels = [request.model.modelId];
      }
      if (request.kind === "image") {
        config.activeImageModel = request.model.modelId;
        config.selectedImageModels = [request.model.modelId];
      }
      gateway.saveConfig(config);
      const response = await fetch(
        `${gateway.baseUrl}${request.path}`,
        request.init,
      );
      if (response.status !== 409)
        throw new Error(`${request.kind}: ${await response.text()}`);
      await assertConsentError(response);
      for (const artifact of spec.artifacts) {
        expect(
          await Bun.file(
            join(gateway.root, "models", request.directory, artifact.filename),
          ).exists(),
        ).toBe(false);
      }
    }

    expect(await gateway.readLlmRuntimeLaunches()).toEqual([]);
    expect(await gateway.readSttRuntimeLaunches()).toEqual([]);
    expect(await gateway.readImageRuntimeLaunches()).toEqual([]);
    expect(gateway.upstreamRequests).toEqual([]);
  } finally {
    await gateway.stop();
  }
});
