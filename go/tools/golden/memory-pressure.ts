// Golden memory-pressure responses from TS: sample sequences on a 64 GiB
// unified host (default reserve: 9.6 GiB; constrained below 12 GiB) run
// through the real MemorySafetyController.poll and MemoryPressureMonitor,
// with applyElevatedMemoryPressure acting on a recording reconciler and video
// job manager. Each step records the controller transition, what the monitor
// did (evict-idle, fail-video, evict-all) and the pressure-change event.
//
//   LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/memory-pressure.ts \
//     > go/internal/runtime/testdata/memory-pressure.json
import { MemorySafetyController } from "../../../src/domains/runtime/memory-controller";
import { MemoryPressureMonitor } from "../../../src/domains/runtime/memory-pressure-monitor";
import {
  defaultMemorySafetyConfig,
  gibibyte,
} from "../../../src/domains/runtime/memory-safety";
import {
  applyElevatedMemoryPressure,
  reportMemoryPressureTransition,
} from "../../../src/domains/runtime/commands/serve";

type Step = Readonly<{
  // Available GiB; null when the reading is unavailable.
  availableGiB: number | null;
  // The provider's own pressure reading (TS providers report normal or unknown).
  pressure?: "normal" | "unknown";
  // How many idle runtimes evictIdleRuntimes stops at this step.
  idle?: number;
  // Whether a memory eviction happened within the reclaim grace.
  grace?: boolean;
}>;

const n = (availableGiB: number | null, more: Omit<Step, "availableGiB"> = {}): Step => ({
  availableGiB,
  ...more,
});

// Shaped after the Oct 9 and Oct 14 incidents (a large load drains memory
// while other runtimes sit idle), plus the hysteresis edges.
const sequences: Record<string, Step[]> = {
  "drain-to-critical-and-recover": [
    n(30), n(20), n(13), n(11.5, { idle: 1 }), n(10.5, { idle: 0 }), n(9),
    n(8), n(11), n(14), n(14), n(14), n(14),
  ],
  "constrained-recovery-resets": [
    n(11, { idle: 1 }), n(13, { idle: 0 }), n(13, { idle: 0 }), n(11, { idle: 0 }),
    n(13, { idle: 0 }), n(13, { idle: 0 }), n(13), n(13),
  ],
  "constrained-nothing-idle": [
    n(11.9, { idle: 0 }), n(11.9, { idle: 0, grace: true }), n(11.9, { idle: 0 }),
    n(12.5, { idle: 0 }),
  ],
  "unknown-samples-hold-state": [
    n(9), n(null, { pressure: "unknown" }), n(14), n(14),
    n(14, { pressure: "unknown" }), n(14), n(14), n(14), n(9),
  ],
  "critical-flaps": [
    n(9), n(9.7), n(9.5), n(12.2), n(9.59), n(9.61), n(20), n(20), n(20),
  ],
  "thresholds": [
    n(12), n(12 - 1 / 1024, { idle: 0 }), n(9.6), n(9.6 - 1 / 1024), n(9.6),
    n(12), n(12), n(12), n(12),
  ],
};

let current: Step = n(30);
const controller = new MemorySafetyController(
  {
    topology: {
      kind: "unified",
      system: { id: "system", capacityBytes: 64 * gibibyte },
      gpuWorkingSetBytes: 48 * gibibyte,
    },
    snapshot: async () => ({
      capturedAtMs: Date.now(),
      pools: [
        current.availableGiB === null
          ? { poolId: "system", availability: "unavailable", pressure: "unknown" }
          : {
              poolId: "system",
              availability: "available",
              availableBytes: Math.round(current.availableGiB * gibibyte),
              pressure: current.pressure ?? "normal",
            },
      ],
    }),
    close: async () => {},
  } as never,
  defaultMemorySafetyConfig(),
);

const out: Record<string, unknown[]> = {};
for (const [name, steps] of Object.entries(sequences)) {
  // A fresh controller per sequence keeps them independent.
  (controller as unknown as { hysteresis: unknown }).hysteresis = {
    state: "healthy",
    consecutiveNormalSnapshots: 0,
  };
  let actions: string[] = [];
  const events: unknown[] = [];
  let transition: unknown;
  const monitor = new MemoryPressureMonitor({
    controller: {
      poll: async () => {
        const t = await controller.poll();
        transition = t;
        return t;
      },
    },
    onElevatedPressure: (t) =>
      applyElevatedMemoryPressure(
        {
          evictIdleRuntimes: async () => {
            actions.push("evict-idle");
            return current.idle ?? 0;
          },
          evictAllRuntimes: async () => {
            actions.push("evict-all");
          },
          isWithinReclaimGrace: () => current.grace ?? false,
        },
        {
          failActiveForMemoryPressure: async () => {
            actions.push("fail-video");
          },
        },
        t,
      ),
    onTransition: (t) =>
      reportMemoryPressureTransition(
        {
          event: (e) => {
            events.push({
              severity: e.severity,
              event: e.eventName,
              attributes: e.attributes,
            });
          },
        },
        t,
      ),
    onError: () => {},
  });
  const results = [];
  for (const step of steps) {
    current = step;
    actions = [];
    events.length = 0;
    await monitor.poll();
    const t = transition as {
      current: { state: string; consecutiveNormalSnapshots: number };
      action: string;
    };
    results.push({
      step,
      state: t.current.state,
      normalSamples: t.current.consecutiveNormalSnapshots,
      action: t.action,
      actions,
      events: [...events],
    });
  }
  out[name] = results;
}

console.log(JSON.stringify({ sequences: out }, null, 2));
