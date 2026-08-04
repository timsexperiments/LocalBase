import { expect, test } from "bun:test";
import { defaultConfig, modelDirectories } from "../../manager";
import type { RuntimeConfigSnapshot } from "./config-snapshot";
import {
  configFieldOwnership,
  createRuntimeReconciliationPlan,
} from "./reconciliation-plan";
import {
  SupervisorRegistry,
  type RuntimeSupervisor,
} from "./supervisor-registry";

const root = "/tmp/local-base-reconciliation";

function snapshot(
  revision: number,
  update: (config: ReturnType<typeof defaultConfig>) => void = () => {},
): RuntimeConfigSnapshot {
  const config = defaultConfig(root, 16);
  update(config);
  Object.freeze(config.selectedLlmModels);
  Object.freeze(config.selectedSttModels);
  Object.freeze(config.selectedImageModels);
  return Object.freeze({ revision, config: Object.freeze(config) });
}

function planFor(
  update: (config: ReturnType<typeof defaultConfig>) => void,
  ownership?: Parameters<typeof createRuntimeReconciliationPlan>[2],
) {
  return createRuntimeReconciliationPlan(
    snapshot(3),
    snapshot(4, update),
    ownership,
  );
}

test("assigns every persisted configuration field to one reconciliation owner", () => {
  expect(configFieldOwnership).toEqual({
    root: "process-identity",
    llmModelsDir: "process-identity",
    sttModelsDir: "process-identity",
    imageModelsDir: "process-identity",
    host: "llm-launch",
    port: "llm-launch",
    ctxSize: "llm-launch",
    sttHost: "stt-launch",
    sttPort: "stt-launch",
    selectedLlmModels: "modality-selection-request-scoped",
    selectedSttModels: "modality-selection-request-scoped",
    selectedImageModels: "modality-selection-request-scoped",
    activeLlmModel: "llm-launch",
    activeSttModel: "stt-launch",
    activeImageModel: "image-launch",
    hfToken: "modality-selection-request-scoped",
    parallel: "llm-launch",
    otelEndpoint: "observability",
    otelHeaders: "observability",
    otelSampleRatio: "observability",
  });
  expect(Object.keys(configFieldOwnership).sort()).toEqual(
    Object.keys(defaultConfig(root)).sort(),
  );
});

test("requires a gateway restart only when process identity changes", () => {
  const nextRoot = "/tmp/local-base-reconciliation-next";
  const plan = planFor((config) => {
    config.root = nextRoot;
    Object.assign(config, modelDirectories(nextRoot));
  });

  expect(plan.processIdentity).toEqual({
    sourceRevision: 3,
    targetRevision: 4,
    action: "restart-required",
    changedFields: ["root", "llmModelsDir", "sttModelsDir", "imageModelsDir"],
  });
  expect(plan.modalities.llm.action).toBe("unchanged");
});

test.each([
  {
    field: "host",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.host = "127.0.0.1";
    },
  },
  {
    field: "port",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.port = 18001;
    },
  },
  {
    field: "ctxSize",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.ctxSize = 8192;
    },
  },
  {
    field: "activeLlmModel",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.activeLlmModel = "mistral-nemo-12b-instruct-q4_k_m";
      config.selectedLlmModels = [config.activeLlmModel];
    },
  },
  {
    field: "parallel",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.parallel = 2;
    },
  },
])("plans an LLM replacement for $field", ({ field, update }) => {
  const plan = planFor(update);
  expect(plan.processIdentity.action).toBe("unchanged");
  expect(plan.modalities.llm.action).toBe("drain-and-replace");
  expect(plan.modalities.llm.changedLaunchFields).toEqual([field]);
});

test.each([
  {
    modality: "stt" as const,
    field: "sttHost",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.sttHost = "127.0.0.1";
    },
  },
  {
    modality: "stt" as const,
    field: "sttPort",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.sttPort = 18081;
    },
  },
  {
    modality: "stt" as const,
    field: "activeSttModel",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.activeSttModel = "whisper-small-q8_0";
      config.selectedSttModels = [config.activeSttModel];
    },
  },
  {
    modality: "image" as const,
    field: "activeImageModel",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.activeImageModel = "stable-diffusion-xl-base-1-0";
      config.selectedImageModels = [config.activeImageModel];
    },
  },
])(
  "plans a $modality replacement for $field",
  ({ modality, field, update }) => {
    const plan = planFor(update);
    expect(plan.modalities[modality].action).toBe("drain-and-replace");
    expect(plan.modalities[modality].changedLaunchFields).toEqual([field]);
  },
);

test.each([
  {
    modality: "stt" as const,
    disable: (config: ReturnType<typeof defaultConfig>) => {
      config.selectedSttModels = [];
      config.activeSttModel = "";
    },
  },
  {
    modality: "image" as const,
    disable: (config: ReturnType<typeof defaultConfig>) => {
      config.selectedImageModels = [];
      config.activeImageModel = "";
    },
  },
])(
  "uses selected $modality models to remove an optional supervisor",
  ({ modality, disable }) => {
    const plan = planFor(disable);
    expect(plan.modalities[modality]).toMatchObject({
      action: "drain-and-remove",
      sourceConfigured: true,
      targetConfigured: false,
    });
  },
);

test.each([
  {
    modality: "stt" as const,
    enable: (config: ReturnType<typeof defaultConfig>) => {
      config.selectedSttModels = [config.activeSttModel];
    },
  },
  {
    modality: "image" as const,
    enable: (config: ReturnType<typeof defaultConfig>) => {
      config.selectedImageModels = [config.activeImageModel];
    },
  },
])(
  "uses selected $modality models to add an optional supervisor",
  ({ modality, enable }) => {
    const source = snapshot(3, (config) => {
      if (modality === "stt") {
        config.selectedSttModels = [];
        config.activeSttModel = "";
      } else {
        config.selectedImageModels = [];
        config.activeImageModel = "";
      }
    });
    const target = snapshot(4, enable);
    const plan = createRuntimeReconciliationPlan(source, target);

    expect(plan.modalities[modality]).toMatchObject({
      action: "add",
      sourceConfigured: false,
      targetConfigured: true,
    });
  },
);

test("keeps LLM configured by default and isolates request-scoped changes", () => {
  const plan = planFor((config) => {
    config.selectedLlmModels = [
      config.activeLlmModel,
      "mistral-nemo-12b-instruct-q4_k_m",
    ];
    config.hfToken = "secret-token";
  });

  expect(plan.modalities.llm).toMatchObject({
    action: "unchanged",
    sourceConfigured: true,
    targetConfigured: true,
  });
  expect(plan.requestScope.changedFields).toEqual([
    "selectedLlmModels",
    "hfToken",
  ]);
  expect(JSON.stringify(plan)).not.toContain("secret-token");
});

test.each([
  {
    field: "otelEndpoint",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.otelEndpoint = "https://otel.example/v1";
    },
  },
  {
    field: "otelHeaders",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.otelHeaders = "authorization=secret";
    },
  },
  {
    field: "otelSampleRatio",
    update: (config: ReturnType<typeof defaultConfig>) => {
      config.otelSampleRatio = 50;
    },
  },
])(
  "plans a separate observability replacement for $field",
  ({ field, update }) => {
    const plan = planFor(update);
    expect(plan.observability).toEqual({
      sourceRevision: 3,
      targetRevision: 4,
      action: "replace",
      changedFields: [field],
    });
  },
);

test("respects serve-time ownership for launch settings", () => {
  const plan = planFor(
    (config) => {
      config.host = "127.0.0.1";
      config.port = 18001;
      config.ctxSize = 8192;
      config.sttHost = "127.0.0.1";
      config.sttPort = 18081;
    },
    {
      configFields: ["host", "port", "ctxSize", "sttHost", "sttPort"],
    },
  );

  expect(plan.modalities.llm.action).toBe("unchanged");
  expect(plan.modalities.stt.action).toBe("unchanged");
});

test("respects serve-time ownership for optional modality selection", () => {
  const source = snapshot(3, (config) => {
    config.selectedSttModels = [];
    config.activeSttModel = "";
  });
  const target = snapshot(4);
  const plan = createRuntimeReconciliationPlan(source, target, {
    configuredModalities: { stt: false },
  });

  expect(plan.modalities.stt).toMatchObject({
    action: "unchanged",
    sourceConfigured: false,
    targetConfigured: false,
  });
});

test("does not allow overrides to mask process identity changes", () => {
  expect(() =>
    createRuntimeReconciliationPlan(snapshot(3), snapshot(4), {
      configFields: ["root"],
    } as unknown as Parameters<typeof createRuntimeReconciliationPlan>[2]),
  ).toThrow("root cannot be owned by an in-process override");
});

test("rejects snapshots with model directories that are not derived from root", () => {
  const invalid = snapshot(4, (config) => {
    config.llmModelsDir = "/tmp/other-models";
  });
  expect(() => createRuntimeReconciliationPlan(snapshot(3), invalid)).toThrow(
    "llmModelsDir must be derived from root",
  );
});

function service(name: string, calls: string[]): RuntimeSupervisor {
  return {
    state: () => "idle",
    async ensureRunning() {},
    async kill() {},
    async shutdown() {
      calls.push(`shutdown:${name}`);
    },
  };
}

test("registry detaches supervisors without stopping them", () => {
  const calls: string[] = [];
  const first = service("first", calls);
  const second = service("second", calls);
  const registry = new SupervisorRegistry({});

  registry.add("stt", first);
  expect(registry.get("stt")).toBe(first);
  expect(() => registry.add("stt", second)).toThrow(
    "stt supervisor is already configured",
  );
  expect(registry.take("stt")).toBe(first);
  expect(registry.get("stt")).toBeUndefined();
  expect(calls).toEqual([]);
  registry.add("stt", second);
  expect(registry.take("stt")).toBe(second);
  expect(registry.take("stt")).toBeUndefined();
  expect(calls).toEqual([]);
});
