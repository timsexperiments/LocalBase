import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { modelsSchema } from "./client";
import { ModelDetails } from "./model-details";

const model = modelsSchema.parse({
  host: {
    memory: {
      kind: "unified",
      system: { capacityBytes: 1024, availableBytes: 512 },
      accelerators: [],
    },
  },
  data: [
    {
      id: "qwen-test",
      catalog: {
        name: "qwen",
        displayName: "Qwen Test",
        kind: "llm",
        quantization: "Q4_K_M",
        qualificationState: "qualified",
        commercialStatus: "open",
        provider: "Qwen",
        source: "https://huggingface.co/Qwen/test",
        revision: "0123456789abcdef0123456789abcdef01234567",
        summary: "A small chat model.",
        memory: {
          minimumVramEstimateGb: 2,
          storageEstimateGb: 1,
          source: "measured",
          reference: {
            source: "measured",
            bytes: 2 * 1024 ** 3,
            launchShape: "ctx=8192;slots=1;kv=f16/f16",
          },
          fit: {
            verdict: "fits",
            source: "measured",
            systemMemoryGb: 64,
            headroomGb: 50,
          },
        },
        inputModalities: ["text"],
        outputModalities: ["text"],
        contextWindowTokens: 32768,
        capabilities: null,
        features: ["tool-calling"],
        capabilityResolution: {
          capabilities: [{ id: "tool-calling", source: "detected" }],
          mismatches: [
            {
              id: "reasoning",
              declared: true,
              detected: false,
              effective: false,
            },
          ],
        },
        artifacts: [
          {
            filename: "model.gguf",
            sizeBytes: 1.2 * 1024 ** 3,
            role: "primary",
          },
        ],
      },
      device: { selected: true, installed: true, runtime: null },
    },
  ],
}).data[0]!;

test("details show provenance, capability sources, memory, and files in plain words", () => {
  const markup = renderToStaticMarkup(
    createElement(ModelDetails, {
      model,
      status: { active: true },
      close: () => {},
      actions: createElement("button", null, "Uninstall"),
    }),
  );
  expect(markup).toContain('aria-labelledby="model-details-title"');
  expect(markup).toContain("Qwen Test");
  expect(markup).toContain("Default");
  expect(markup).toContain("Commercial use OK");
  expect(markup).toContain("at revision 0123456789ab");
  expect(markup).toContain("32,768 tokens");
  expect(markup).toContain("Uninstall");
  expect(markup).toContain("model.gguf");
  expect(markup).toContain("1.2 GB");
  expect(markup).toContain("Fits, 50 GB to spare of 64 GB");
  expect(markup).toContain("8,192-token context");
  expect(markup).not.toContain("ctx=8192");
});

test("details explain a capability mismatch", () => {
  const markup = renderToStaticMarkup(
    createElement(ModelDetails, {
      model,
      status: undefined,
      close: () => {},
      actions: null,
    }),
  );
  expect(markup).toContain("the installed files don&#x27;t support it");
  expect(markup).not.toContain(">Default<");
});

test("details name the feed a community entry came from", () => {
  const community = {
    ...model,
    catalog: {
      ...model.catalog,
      trust: "community" as const,
      catalogTier: "remote" as const,
      catalogFeed: "https://feeds.example.com/catalog/feed.json",
    },
  };
  const markup = renderToStaticMarkup(
    createElement(ModelDetails, {
      model: community,
      status: undefined,
      close: () => {},
      actions: null,
    }),
  );
  expect(markup).toContain("Community");
  expect(markup).toContain("Remote catalog");
  expect(markup).toContain("Catalog feed");
  expect(markup).toContain("https://feeds.example.com/catalog/feed.json");
});
