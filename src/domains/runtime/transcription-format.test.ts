import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";
import { minimalWav } from "../../test/media-fixtures";
import { latestUpstreamMultipartFormData } from "../../test/ai-sdk-conformance";

describe("transcription response formats", () => {
  let gateway: GatewayFixture;
  beforeAll(async () => {
    gateway = await startGatewayFixture();
  }, 30_000);
  afterAll(async () => {
    await gateway?.stop();
  }, 10_000);

  function post(
    fields: Record<string, string>,
    route = "transcriptions",
  ): Promise<Response> {
    const form = new FormData();
    form.append(
      "file",
      new File([minimalWav], "fixture.wav", { type: "audio/wav" }),
    );
    if (!("prompt" in fields)) form.append("prompt", "messy");
    for (const [key, value] of Object.entries(fields)) form.append(key, value);
    return fetch(`${gateway.baseUrl}/v1/audio/${route}`, {
      method: "POST",
      body: form,
    });
  }

  test("json returns one clean text string", async () => {
    const response = await post({ response_format: "json" });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("application/json");
    expect(await response.json()).toEqual({ text: "Hello there. And then..." });
    expect(
      latestUpstreamMultipartFormData(gateway).get("response_format"),
    ).toBe("json");
  });

  test("omitted response_format defaults to json", async () => {
    const response = await post({});
    expect(response.headers.get("content-type")).toContain("application/json");
    expect(await response.json()).toEqual({ text: "Hello there. And then..." });
  });

  test("text returns normalized text/plain", async () => {
    const response = await post({ response_format: "text" });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/plain");
    expect(await response.text()).toBe("Hello there. And then...");
  });

  test("verbose_json normalizes text and each segment", async () => {
    const response = await post({ response_format: "verbose_json" });
    expect(response.headers.get("content-type")).toContain("application/json");
    const body = (await response.json()) as {
      task: string;
      text: string;
      segments: Array<{ text: string }>;
    };
    expect(body.task).toBe("transcribe");
    expect(body.text).toBe("Hello there. And then...");
    expect(body.segments.map((segment) => segment.text)).toEqual([
      "Hello there.",
      "And then...",
    ]);
  });

  test("srt and vtt pass through untouched as text/plain", async () => {
    const srt = await post({ response_format: "srt" });
    expect(srt.headers.get("content-type")).toContain("text/plain");
    expect(await srt.text()).toBe(
      "1\n00:00:00,000 --> 00:00:01,000\n Hello there.\n\n",
    );
    const vtt = await post({ response_format: "vtt" });
    expect(vtt.headers.get("content-type")).toContain("text/plain");
    expect(await vtt.text()).toBe(
      "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n Hello there.\n\n",
    );
  });

  test.each(["text", "srt", "vtt"])(
    "%s sanitizes a 200 JSON error body from the backend",
    async (format) => {
      const response = await post({
        response_format: format,
        prompt: "backend-error",
      });
      expect(response.status).toBe(502);
      expect(response.headers.get("content-type")).toContain(
        "application/json",
      );
      const raw = await response.text();
      expect(raw).not.toContain("private backend failed");
      expect(JSON.parse(raw).error.code).toBe("upstream_error");
    },
  );

  test("translations sets translate=true; transcriptions does not", async () => {
    const translated = await post(
      { response_format: "verbose_json" },
      "translations",
    );
    expect(((await translated.json()) as { task: string }).task).toBe(
      "translate",
    );
    expect(latestUpstreamMultipartFormData(gateway).get("translate")).toBe(
      "true",
    );
    await post({ response_format: "json" });
    expect(
      latestUpstreamMultipartFormData(gateway).get("translate"),
    ).toBeNull();
  });
});
