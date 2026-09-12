import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createOpenAI } from "@ai-sdk/openai";
import { generateSpeech } from "ai";
import { readdir, rm } from "node:fs/promises";
import { join } from "node:path";
import { readLogSnapshot } from "../observability/logging";
import {
  startGatewayFixture,
  TTS_MODEL,
  type GatewayFixture,
  waitForLogEvent,
} from "../../test/gateway-fixture";
import { validateSpeechWav } from "./speech-supervisor";

function speechBody(overrides: Record<string, unknown> = {}) {
  return {
    model: TTS_MODEL,
    input: "Hello from the speech fixture.",
    voice: "default",
    response_format: "wav",
    speed: 1,
    ...overrides,
  };
}

describe("OpenAI speech endpoint", () => {
  let gateway: GatewayFixture;

  beforeAll(
    async () => {
      gateway = await startGatewayFixture({
        auth: { mode: "bearer" },
        ttsEnabled: true,
      });
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await gateway?.stop();
    },
    { timeout: 10_000 },
  );

  test("rejects authentication and unsupported settings before starting a child", async () => {
    const before = (await gateway.readTtsRuntimeEvents()).length;
    const inferenceBefore = (await readLogSnapshot(gateway.root)).filter(
      ({ eventName }) => eventName === "inference.completed",
    ).length;
    const unauthorized = await fetch(`${gateway.baseUrl}/v1/audio/speech`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(speechBody()),
    });
    expect(unauthorized.status).toBe(401);

    if (!gateway.apiKey) throw new Error("Expected fixture API key.");
    const headers = {
      authorization: `Bearer ${gateway.apiKey}`,
      "content-type": "application/json",
    };
    for (const [overrides, expected] of [
      [{ response_format: undefined }, "response_format"],
      [{ response_format: "mp3" }, "explicitly set to 'wav'"],
      [{ voice: "alloy" }, "voice must be 'default'"],
      [{ speed: 1.25 }, "speed must be 1"],
      [{ instructions: "Whisper" }, "instructions are not supported"],
      [{ input: "x".repeat(257) }, "256 characters"],
    ] as const) {
      const response = await fetch(`${gateway.baseUrl}/v1/audio/speech`, {
        method: "POST",
        headers,
        body: JSON.stringify(speechBody(overrides)),
      });
      expect(response.status).toBe(400);
      expect(JSON.stringify(await response.json())).toContain(expected);
    }
    expect(await gateway.readTtsRuntimeEvents()).toHaveLength(before);
    expect(
      (await readLogSnapshot(gateway.root)).filter(
        ({ eventName }) => eventName === "inference.completed",
      ),
    ).toHaveLength(inferenceBefore);
  });

  test("returns actual WAV bytes through AI SDK generateSpeech", async () => {
    if (!gateway.apiKey) throw new Error("Expected fixture API key.");
    const openai = createOpenAI({
      baseURL: `${gateway.baseUrl}/v1`,
      apiKey: gateway.apiKey,
      name: "localbase",
    });
    const result = await generateSpeech({
      model: openai.speech(TTS_MODEL),
      text: "Hello from the AI SDK.",
      voice: "default",
      outputFormat: "wav",
      maxRetries: 0,
    });

    expect(result.audio.mediaType).toBe("audio/wav");
    expect(validateSpeechWav(result.audio.uint8Array).sampleCount).toBe(3_840);
    const events = await gateway.readTtsRuntimeEvents();
    const started = events.find(({ event }) => event === "started");
    expect(started?.promptLength).toBe(22);
    expect(started?.args).not.toContain("Hello from the AI SDK.");
    expect(started?.args).toEqual(
      expect.arrayContaining(["-m", expect.stringContaining("Q4_K_M.gguf")]),
    );
    expect(started?.args).toEqual(
      expect.arrayContaining(["-mm", expect.stringContaining("Q8_0.gguf")]),
    );
    const event = await waitForLogEvent(
      gateway,
      (candidate) =>
        candidate.eventName === "inference.completed" &&
        candidate.runtime === "tts",
    );
    expect(event.attributes).toMatchObject({
      model_id: TTS_MODEL,
      runtime_name: "llama-tts",
      outcome: "completed",
      http_status: 200,
    });
    expect(event.attributes).not.toHaveProperty("total_tokens");
  });
});

test(
  "hot enable admits the installed TTS model without restarting the gateway",
  async () => {
    const gateway = await startGatewayFixture({ ttsInstalled: true });
    try {
      const before = await fetch(`${gateway.baseUrl}/v1/audio/speech`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(speechBody()),
      });
      expect(before.status).toBe(501);

      const enabled = gateway.readConfig();
      enabled.selectedTtsModels = [TTS_MODEL];
      enabled.activeTtsModel = TTS_MODEL;
      gateway.saveConfig(enabled);

      const response = await fetch(`${gateway.baseUrl}/v1/audio/speech`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(speechBody()),
      });
      expect(response.status).toBe(200);
      expect(
        validateSpeechWav(new Uint8Array(await response.arrayBuffer()))
          .sampleCount,
      ).toBe(3_840);
      expect(await gateway.readTtsRuntimeEvents()).toContainEqual(
        expect.objectContaining({ event: "started" }),
      );
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 30_000 },
);

test(
  "hot disable cancels only TTS while an admitted LLM peer completes",
  async () => {
    const releasePath = `/tmp/localbase-speech-release-${crypto.randomUUID()}`;
    const gateway = await startGatewayFixture({
      ttsEnabled: true,
      ttsControl: { mode: "hold", releasePath },
    });
    try {
      const llm = fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-test-upstream": "controlled-stream",
          "x-test-stream-id": "speech-peer",
        },
        body: JSON.stringify({
          model: gateway.readConfig().activeLlmModel,
          messages: [{ role: "user", content: "Keep this peer alive." }],
          stream: true,
        }),
      });
      await gateway.waitForUpstreamRequest("speech-peer");

      const speech = fetch(`${gateway.baseUrl}/v1/audio/speech`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(speechBody()),
      });
      await gateway.waitForTtsRuntimeEvent("started");
      const disabled = gateway.readConfig();
      disabled.selectedTtsModels = [];
      disabled.activeTtsModel = "";
      gateway.saveConfig(disabled);

      const rejected = await fetch(`${gateway.baseUrl}/v1/audio/speech`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(speechBody()),
      });
      expect(rejected.status).toBe(501);
      const speechResponse = await speech;
      expect(speechResponse.status).toBe(499);
      const requestId = speechResponse.headers.get("x-localbase-request-id");
      expect(requestId).toMatch(/^lbreq_/);
      const event = await waitForLogEvent(
        gateway,
        (candidate) =>
          candidate.eventName === "inference.completed" &&
          candidate.requestId === requestId,
      );
      expect(event.attributes).toMatchObject({
        outcome: "cancelled",
        http_status: 499,
        terminal_source: "request_aborted",
      });
      expect(
        (await readLogSnapshot(gateway.root)).filter(
          (candidate) =>
            candidate.eventName === "inference.completed" &&
            candidate.requestId === requestId,
        ),
      ).toHaveLength(1);

      gateway.closeControlledStream("speech-peer");
      const llmResponse = await llm;
      expect(llmResponse.status).toBe(200);
      expect(await llmResponse.text()).toContain("[DONE]");
    } finally {
      await gateway.stop();
      await Bun.file(releasePath)
        .delete()
        .catch(() => {});
    }
  },
  { timeout: 30_000 },
);

test(
  "gateway shutdown kills an active TTS child and removes private files",
  async () => {
    const gateway = await startGatewayFixture({ ttsEnabled: true });
    const releasePath = join(gateway.root, "hold-speech");
    await gateway.setTtsRuntimeControl({ mode: "hold", releasePath });
    const speech = fetch(`${gateway.baseUrl}/v1/audio/speech`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(speechBody()),
    }).catch((error: unknown) => error);
    let stopped = false;
    try {
      await gateway.waitForTtsRuntimeEvent("started");
      await gateway.stop({ preserveRoot: true });
      stopped = true;
      await speech;
      expect(await readdir(join(gateway.root, "tmp"))).toEqual([]);
    } finally {
      if (!stopped) await gateway.stop({ preserveRoot: true });
      await rm(gateway.root, { recursive: true, force: true });
    }
  },
  { timeout: 30_000 },
);
