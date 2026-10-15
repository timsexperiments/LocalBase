import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import {
  createRuntimeCapabilityStore,
  parseLlamaServerProps,
  probeLlamaServerCapabilities,
  type RuntimeCapabilityRecord,
} from "./runtime-capabilities";

const session = new DatabaseSession();
const root = mkdtempSync(join(tmpdir(), "local-base-runtime-capabilities-"));
afterAll(() => {
  session.close();
  rmSync(root, { recursive: true, force: true });
});

function store() {
  return createRuntimeCapabilityStore(
    session.get(mkdtempSync(join(root, "db-"))),
  );
}

describe("parseLlamaServerProps", () => {
  test("reads tool support, thinking templates, and modalities", () => {
    expect(
      parseLlamaServerProps(
        {
          build_info: "b10419-abc",
          chat_template: "{% if enable_thinking %}<think>{% endif %}",
          chat_template_caps: { supports_tool_calls: true },
          modalities: { vision: true, audio: false },
          extra: 1,
        },
        { embedding: false },
      ),
    ).toEqual({
      capabilities: { "tool-calling": true, reasoning: true },
      inputModalities: ["text", "image"],
      buildInfo: "b10419-abc",
    });
  });

  test("embedders report no chat capabilities", () => {
    expect(
      parseLlamaServerProps(
        {
          chat_template: "<think>",
          chat_template_caps: { supports_tool_calls: true },
          modalities: { audio: true },
        },
        { embedding: true },
      ),
    ).toEqual({
      capabilities: {},
      inputModalities: ["text", "audio"],
      buildInfo: "unknown",
    });
  });

  test("rejects malformed bodies", () => {
    expect(parseLlamaServerProps("nope", { embedding: false })).toBeNull();
    expect(
      parseLlamaServerProps({ modalities: "x" }, { embedding: false }),
    ).toBeNull();
  });
});

describe("runtime capability store", () => {
  const record: RuntimeCapabilityRecord = {
    modelId: "qwen",
    artifactSha256: "a".repeat(64),
    capabilities: { "tool-calling": false },
    inputModalities: ["text"],
    buildInfo: "b1",
    observedAt: "2026-10-15T00:00:00.000Z",
  };

  test("round-trips and upserts per model and artifact hash", () => {
    const capabilities = store();
    expect(capabilities.read("qwen", record.artifactSha256)).toBeNull();
    capabilities.record(record);
    expect(capabilities.read("qwen", record.artifactSha256)).toEqual(record);

    const updated = {
      ...record,
      capabilities: { "tool-calling": true, reasoning: true },
      buildInfo: "b2",
      observedAt: "2026-10-16T00:00:00.000Z",
    };
    capabilities.record(updated);
    expect(capabilities.read("qwen", record.artifactSha256)).toEqual(updated);
    expect(capabilities.read("qwen", "b".repeat(64))).toBeNull();
  });
});

describe("probeLlamaServerCapabilities", () => {
  test("stores what a healthy server reports", async () => {
    const capabilities = store();
    const urls: string[] = [];
    const result = await probeLlamaServerCapabilities({
      baseUrl: "http://127.0.0.1:24999",
      modelId: "qwen",
      artifactSha256: "c".repeat(64),
      embedding: false,
      store: capabilities,
      now: () => new Date("2026-10-15T00:00:00.000Z"),
      fetch: async (url) => {
        urls.push(url);
        return Response.json({
          build_info: "b3",
          chat_template_caps: { supports_tool_calls: false },
        });
      },
    });
    expect(urls).toEqual(["http://127.0.0.1:24999/props"]);
    expect(result).toEqual({
      modelId: "qwen",
      artifactSha256: "c".repeat(64),
      capabilities: { "tool-calling": false },
      inputModalities: ["text"],
      buildInfo: "b3",
      observedAt: "2026-10-15T00:00:00.000Z",
    });
    expect(capabilities.read("qwen", "c".repeat(64))).toEqual(result!);
  });

  test("stores nothing when the server answers with an error", async () => {
    const capabilities = store();
    const result = await probeLlamaServerCapabilities({
      baseUrl: "http://127.0.0.1:24999",
      modelId: "qwen",
      artifactSha256: "d".repeat(64),
      embedding: false,
      store: capabilities,
      fetch: async () => new Response("no", { status: 503 }),
    });
    expect(result).toBeNull();
    expect(capabilities.read("qwen", "d".repeat(64))).toBeNull();
  });
});
