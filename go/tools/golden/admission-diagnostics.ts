// Golden `runtime.memory-admission-rejected` attributes from TS: scripted
// operations on a real MemorySafetyController (64 GiB unified host, 48 GiB
// GPU working set, default reserve) and the memoryAdmissionRejectedAttributes
// of each refusal. The Go test replays the same operations.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/admission-diagnostics.ts \
//     > go/internal/memory/testdata/admission-diagnostics.json
import {
  MemorySafetyController,
  memoryAdmissionRejectedAttributes,
  RuntimeMemoryAdmissionError,
} from "../../../src/domains/runtime/memory-controller";
import {
  defaultMemorySafetyConfig,
  gibibyte,
  type RuntimeMemoryDemand,
} from "../../../src/domains/runtime/memory-safety";

type Demand = Readonly<{
  unifiedGiB: number;
  acceleratorGiB?: number;
  confidence?: RuntimeMemoryDemand["confidence"];
}>;
type Op =
  | { op: "available"; gib: number | null }
  | { op: "poll" }
  | { op: "reserve"; id: string; demand: Demand; materialize?: boolean }
  | { op: "release"; id: string }
  | { op: "check"; demand: Demand; releasing?: string[] }
  | { op: "start"; id: string; demand: Demand };

const d = (
  unifiedGiB: number,
  acceleratorGiB = unifiedGiB,
  confidence: RuntimeMemoryDemand["confidence"] = "measured",
): Demand => ({ unifiedGiB, acceleratorGiB, confidence });

const scenarios: Record<string, Op[]> = {
  "system-memory-with-pending": [
    { op: "available", gib: 30 },
    { op: "reserve", id: "llm", demand: d(8) },
    { op: "check", demand: d(14, 14, "estimated") },
  ],
  "system-memory-switch-credit": [
    { op: "available", gib: 20 },
    { op: "reserve", id: "image", demand: d(6), materialize: true },
    { op: "check", demand: d(17), releasing: ["image"] },
    { op: "check", demand: d(15), releasing: ["image"] },
  ],
  "observed-pressure": [
    { op: "available", gib: 11 },
    { op: "check", demand: d(1) },
  ],
  "hysteresis-pressure": [
    { op: "available", gib: 5 },
    { op: "poll" },
    { op: "available", gib: 40 },
    { op: "poll" },
    { op: "check", demand: d(1) },
    { op: "start", id: "stt", demand: d(1) },
  ],
  "measurement-unavailable": [
    { op: "available", gib: null },
    { op: "check", demand: d(2) },
  ],
  "gpu-working-set-oversubscribed": [
    { op: "available", gib: 60 },
    { op: "reserve", id: "llm", demand: d(30), materialize: true },
    { op: "check", demand: d(20) },
    { op: "check", demand: d(20), releasing: ["llm"] },
  ],
  "gpu-working-set-permanent": [
    { op: "available", gib: 60 },
    { op: "check", demand: d(50, 50, "estimated") },
  ],
  "system-capacity-permanent": [
    { op: "available", gib: 60 },
    { op: "check", demand: d(56, 10) },
  ],
  "start-rejected": [
    { op: "available", gib: 20 },
    { op: "start", id: "image", demand: d(12) },
    { op: "start", id: "image", demand: d(4) },
  ],
};

let available: number | null = 30;
function fresh() {
  return new MemorySafetyController(
    {
      topology: {
        kind: "unified",
        system: { id: "system", capacityBytes: 64 * gibibyte },
        gpuWorkingSetBytes: 48 * gibibyte,
      },
      snapshot: async () => ({
        capturedAtMs: Date.now(),
        pools: [
          available === null
            ? { poolId: "system", availability: "unavailable", pressure: "unknown" }
            : {
                poolId: "system",
                availability: "available",
                availableBytes: Math.round(available * gibibyte),
                pressure: "normal",
              },
        ],
      }),
      close: async () => {},
    } as never,
    defaultMemorySafetyConfig(),
  );
}

const bytes = (gib: number) => Math.round(gib * gibibyte);
const demandOf = (x: Demand): RuntimeMemoryDemand => ({
  unifiedBytes: bytes(x.unifiedGiB),
  hostBytes: bytes(x.unifiedGiB),
  acceleratorBytes: bytes(x.acceleratorGiB ?? x.unifiedGiB),
  confidence: x.confidence ?? "measured",
});
const attributes = (error: RuntimeMemoryAdmissionError) => {
  const { sample_captured_at_ms, sample_age_ms, ...rest } =
    memoryAdmissionRejectedAttributes(error);
  // Timestamps differ per run: only their presence is pinned.
  return {
    ...rest,
    sample_captured_at_ms: typeof sample_captured_at_ms,
    sample_age_ms: typeof sample_age_ms,
  };
};

const out: Record<string, unknown> = {};
for (const [name, ops] of Object.entries(scenarios)) {
  const controller = fresh();
  const reservations = new Map<string, { release(): void }>();
  const results: unknown[] = [];
  for (const op of ops) {
    switch (op.op) {
      case "available":
        available = op.gib;
        break;
      case "poll":
        await controller.poll();
        break;
      case "reserve": {
        const r = await controller.reserve({
          runtimeId: op.id,
          demand: demandOf(op.demand),
        });
        if (op.materialize) r.materialize(4242);
        reservations.set(op.id, r);
        break;
      }
      case "release":
        reservations.get(op.id)?.release();
        break;
      case "check": {
        const rejection = await controller.checkAdmission(
          { demand: demandOf(op.demand) },
          { releasingRuntimeIds: op.releasing ?? [] },
        );
        results.push(rejection ? attributes(rejection) : null);
        break;
      }
      case "start": {
        try {
          const r = await controller.reserve({
            runtimeId: op.id,
            demand: demandOf(op.demand),
          });
          r.release();
          results.push(null);
        } catch (error) {
          if (!(error instanceof RuntimeMemoryAdmissionError)) throw error;
          results.push(attributes(error));
        }
        break;
      }
    }
  }
  out[name] = { ops, results };
}

console.log(JSON.stringify({ scenarios: out }, null, 2));
