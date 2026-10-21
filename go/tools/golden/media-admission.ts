// Golden switch-preflight figures from TS for catalog image and stt models
// whose files are not installed: the demand the TS supervisor-factory
// preflight resolves from the catalog (primary artifact filename and
// expectedSizeBytes), and the MemorySafetyController.checkAdmission verdict
// for that demand on a 64 GiB unified host with two resident runtimes, at a
// grid of available-memory readings plus each model's exact boundary.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/media-admission.ts \
//     > go/internal/runtime/testdata/media-admission.json
import { CATALOG, primaryArtifact } from "../../../src/catalog";
import {
  resolveImageLaunchPlan,
  resolveSttLaunchPlan,
} from "../../../src/domains/runtime/launch-plan";
import { withAdmissionDemand } from "../../../src/domains/runtime/admission-demand";
import { MemorySafetyController } from "../../../src/domains/runtime/memory-controller";
import {
  defaultMemorySafetyConfig,
  effectiveMemoryReserveBytes,
  gibibyte,
  type RuntimeMemoryDemand,
} from "../../../src/domains/runtime/memory-safety";
import { keepFormerCatalogEntries } from "../../../src/test/former-catalog";

// Installs keep former built-in entries (#368); plan them as well.
keepFormerCatalogEntries();

const capacityBytes = 64 * gibibyte;
const gpuWorkingSetBytes = 48 * gibibyte;
// Stand-ins for the resident whisper and embedding runtimes of a full
// contract run. Resident reservations are part of the measured sample, so
// they only count toward the GPU working-set sum.
const residents: Record<string, RuntimeMemoryDemand> = {
  stt: {
    unifiedBytes: 600 * 1024 ** 2,
    hostBytes: 600 * 1024 ** 2,
    acceleratorBytes: 600 * 1024 ** 2,
    confidence: "measured",
  },
  llm: {
    unifiedBytes: 2 * gibibyte,
    hostBytes: 2 * gibibyte,
    acceleratorBytes: 2 * gibibyte,
    confidence: "measured",
  },
};

let available = 60 * gibibyte;
const controller = new MemorySafetyController(
  {
    topology: {
      kind: "unified",
      system: { id: "system", capacityBytes },
      gpuWorkingSetBytes,
    },
    snapshot: async () => ({
      capturedAtMs: Date.now(),
      pools: [
        {
          poolId: "system",
          availability: "available",
          availableBytes: available,
          pressure: "normal",
        },
      ],
    }),
    close: async () => {},
  },
  defaultMemorySafetyConfig(),
);
for (const [runtimeId, demand] of Object.entries(residents)) {
  (await controller.reserve({ runtimeId, demand })).materialize();
}

const reserveBytes = effectiveMemoryReserveBytes(
  defaultMemorySafetyConfig().systemReserve,
  capacityBytes,
);
const grid: number[] = [];
for (let tenths = 100; tenths <= 400; tenths += 5) {
  grid.push(Math.round((tenths / 10) * gibibyte));
}

function preflightDemand(
  spec: (typeof CATALOG)[number],
): RuntimeMemoryDemand {
  const primary = primaryArtifact(spec);
  const common = {
    runtimeId: spec.kind,
    root: "/nonexistent",
    modelsDirectory: `/nonexistent/models/${spec.kind}`,
    modelId: spec.modelId,
    modelFile: primary.filename,
    host: "127.0.0.1",
    port: 1,
    model: spec,
  };
  if (spec.kind === "stt") {
    const plan = resolveSttLaunchPlan({
      ...common,
      artifactBytes: primary.expectedSizeBytes!,
    });
    return withAdmissionDemand(plan, spec).memoryDemand;
  }
  const plan = resolveImageLaunchPlan({
    ...common,
    imageRuntime: spec.imageRuntime,
    artifactBytes: spec.imageRuntime
      ? spec.artifacts.reduce(
          (total, artifact) => total + (artifact.expectedSizeBytes ?? 0),
          0,
        )
      : primary.expectedSizeBytes!,
  });
  return withAdmissionDemand(plan, spec).memoryDemand;
}

const models = [];
for (const spec of CATALOG) {
  if (spec.kind !== "image" && spec.kind !== "stt") continue;
  const demand = preflightDemand(spec);
  const boundary = reserveBytes + demand.unifiedBytes;
  const readings = [...grid, boundary - 1, boundary];
  const verdicts = [];
  for (const reading of readings) {
    available = reading;
    const rejection = await controller.checkAdmission(
      { demand },
      { releasingRuntimeIds: [spec.kind] },
    );
    verdicts.push({
      availableBytes: reading,
      verdict: rejection ? rejection.decision.reason : "admitted",
    });
  }
  models.push({ modelId: spec.modelId, kind: spec.kind, demand, verdicts });
}

console.log(
  JSON.stringify(
    {
      capacityBytes,
      gpuWorkingSetBytes,
      reserveBytes,
      residents,
      models,
    },
    null,
    1,
  ),
);
