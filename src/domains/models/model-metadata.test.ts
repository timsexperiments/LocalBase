import { expect, test } from "bun:test";
import { byId } from "../../catalog";
import { defaultConfig } from "../../manager";
import { createRuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import {
  modelMetadataSchema,
  projectModelMetadata,
  projectModelMetadataList,
} from "./model-metadata";

const modelId = "qwen2.5-coder-1.5b-instruct-q4_k_m";

function catalogModel() {
  const model = byId(modelId);
  if (!model) throw new Error(`Expected catalog model ${modelId}.`);
  return model;
}

function runtimeSnapshots() {
  return {
    llm: createRuntimeLifecycleSnapshot({
      modality: "llm",
      configured: true,
      state: "running",
      modelId,
      runtimeId: "llm:test:1",
      admission: { kind: "unknown" },
      configuredSlots: 4,
    }),
    stt: createRuntimeLifecycleSnapshot({
      modality: "stt",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
    image: createRuntimeLifecycleSnapshot({
      modality: "image",
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    }),
  };
}

test("projects catalog facts separately from observed device state", () => {
  const config = defaultConfig("/tmp/localbase-model-metadata");
  config.selectedLlmModels = [modelId];
  config.selectedSttModels = [];
  config.selectedImageModels = [];
  const metadata = projectModelMetadata(catalogModel(), {
    catalog: [catalogModel()],
    config,
    installations: new Map([[modelId, true]]),
    runtimes: runtimeSnapshots(),
  });

  expect(metadata).toMatchObject({
    object: "localbase.model",
    id: modelId,
    catalog: {
      name: "Qwen2.5-Coder",
      quantization: "Q4_K_M",
      capabilities: null,
      contextWindowTokens: null,
      maxOutputTokens: null,
    },
    device: {
      selected: true,
      installed: true,
      runtime: { configured: true, state: "running" },
      warm: null,
      slots: null,
      readiness: null,
      queue: null,
    },
  });
  expect(metadata.catalog.artifact.sha256).toMatch(/^[a-f0-9]{64}$/);
  expect(metadata.catalog.artifact.sizeBytes).toBeGreaterThan(0);
  expect(metadata.catalog.revision).toMatch(/^[a-f0-9]{40}$/);
});

test("uses the strict response schemas for lists and entries", () => {
  const model = catalogModel();
  const config = defaultConfig("/tmp/localbase-model-metadata-strict");
  const input = {
    catalog: [model],
    config,
    installations: new Map([[modelId, false]]),
    runtimes: runtimeSnapshots(),
  };
  const list = projectModelMetadataList(input);

  expect(list.data).toHaveLength(1);
  expect(
    modelMetadataSchema.safeParse({ ...list.data[0], extra: true }).success,
  ).toBe(false);
});
