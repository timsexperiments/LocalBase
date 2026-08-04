import { describe, expect, test } from "bun:test";
import {
  resolveImageLaunchPlan,
  resolveLlmLaunchPlan,
  resolveSttLaunchPlan,
} from "./launch-plan";
import { SupervisorRegistry } from "./supervisor-registry";

const root = "/tmp/local-base";

describe("runtime launch plans", () => {
  test.each([
    {
      name: "llm",
      resolve: () =>
        resolveLlmLaunchPlan({
          root,
          modelsDirectory: `${root}/models/llm`,
          modelId: "model",
          modelFile: "model.gguf",
          host: "127.0.0.1",
          port: 8080,
          ctxSize: 8192,
          parallel: "auto",
          modelRequirementGb: 4,
          hardware: { memoryGb: 16 },
        }),
      expected: {
        modality: "llm",
        component: "llama-server",
        modelPath: `${root}/models/llm/model.gguf`,
        healthUrl: "http://127.0.0.1:8080/health",
      },
    },
    {
      name: "stt",
      resolve: () =>
        resolveSttLaunchPlan({
          root,
          modelsDirectory: `${root}/models/stt`,
          modelId: "model",
          modelFile: "model.bin",
          host: "127.0.0.1",
          port: 8081,
        }),
      expected: {
        modality: "stt",
        component: "whisper-server",
        modelPath: `${root}/models/stt/model.bin`,
        healthUrl: "http://127.0.0.1:8081/health",
      },
    },
    {
      name: "image",
      resolve: () =>
        resolveImageLaunchPlan({
          root,
          modelsDirectory: `${root}/models/image`,
          modelId: "model",
          modelFile: "model.safetensors",
          host: "127.0.0.1",
          port: 8082,
        }),
      expected: {
        modality: "image",
        component: "sd-server",
        modelPath: `${root}/models/image/model.safetensors`,
        healthUrl: "http://127.0.0.1:8082/",
      },
    },
  ])("resolves $name launch settings without I/O", ({ resolve, expected }) => {
    expect(resolve()).toMatchObject(expected);
  });

  test("detaches and freezes launch inputs", () => {
    const hardware = { memoryGb: 16 };
    const plan = resolveLlmLaunchPlan({
      root,
      modelsDirectory: `${root}/models/llm`,
      modelId: "model",
      modelFile: "model.gguf",
      host: "127.0.0.1",
      port: 8080,
      ctxSize: 8192,
      parallel: "auto",
      modelRequirementGb: 4,
      hardware,
    });
    hardware.memoryGb = 32;

    expect(Object.isFrozen(plan)).toBe(true);
    expect(Object.isFrozen(plan.hardware)).toBe(true);
    expect(plan.hardware.memoryGb).toBe(16);
  });
});

test("supervisor registry reports configured state and shuts down each supervisor", async () => {
  let shutdowns = 0;
  const service = {
    state: () => "running" as const,
    async ensureRunning() {},
    async kill() {},
    async shutdown() {
      shutdowns += 1;
    },
  };
  const registry = new SupervisorRegistry({
    llm: service,
    image: service,
  });

  expect(registry.state("llm", true)).toEqual({
    configured: true,
    state: "running",
  });
  expect(registry.state("stt", false)).toEqual({
    configured: false,
    state: "disabled",
  });
  await registry.shutdown();
  expect(shutdowns).toBe(2);
});
