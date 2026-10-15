import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { byId, primaryArtifact } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import type { RuntimeLaunchPlan } from "./launch-plan";
import {
  DEFAULT_LAUNCH_SHAPE,
  createObservedMemoryStore,
  llmLaunchShape,
  observationTarget,
  observePeakMemory,
  parseFootprintTotal,
  parseVmHwm,
  planLaunchShape,
  type MemorySample,
  type ObservedMemoryRecord,
} from "./observed-memory";

const session = new DatabaseSession();
const root = mkdtempSync(join(tmpdir(), "local-base-observed-memory-"));
afterAll(() => {
  session.close();
  rmSync(root, { recursive: true, force: true });
});

function store() {
  return createObservedMemoryStore(session.get(mkdtempSync(join(root, "db-"))));
}

const sha = "a".repeat(64);

function record(
  overrides: Partial<ObservedMemoryRecord> = {},
): ObservedMemoryRecord {
  return {
    modelId: "test-model",
    artifactSha256: sha,
    launchShape: "ctx=8192;slots=1;kv=q8_0/q8_0",
    peakBytes: 2 * 1024 ** 3,
    method: "footprint",
    observedAt: "2026-10-15T00:00:00.000Z",
    ...overrides,
  };
}

const FOOTPRINT_OUTPUT = `======================================================================
llama-server [28256]: 64-bit    Footprint: 2016 KB (16384 bytes per page)
======================================================================

  Dirty      Clean  Reclaimable    Regions    Category
    ---        ---          ---        ---    ---
 720 KB        0 B          0 B          5    MALLOC_SMALL
    0 B     1.5 GB          0 B         50    mapped file
    ---        ---          ---        ---    ---
1.2 GB     1.5 GB          0 B        410    TOTAL

Auxiliary data:
`;

describe("parsers", () => {
  test("footprint TOTAL adds dirty and clean pages", () => {
    expect(parseFootprintTotal(FOOTPRINT_OUTPUT)).toBe(
      Math.round(1.2 * 1024 ** 3 + 1.5 * 1024 ** 3),
    );
    expect(parseFootprintTotal("no totals here")).toBeNull();
  });

  test("VmHWM is read in kB", () => {
    expect(parseVmHwm("Name:\tllama\nVmHWM:\t  2048 kB\nVmRSS:\t1 kB\n")).toBe(
      2048 * 1024,
    );
    expect(parseVmHwm("VmRSS:\t1 kB\n")).toBeNull();
  });
});

describe("observed memory store", () => {
  test("keeps the larger peak per model, artifact, and launch shape", () => {
    const peaks = store();
    peaks.record(record({ peakBytes: 3 * 1024 ** 3 }));
    peaks.record(
      record({ peakBytes: 2 * 1024 ** 3, observedAt: "2026-10-16T00:00:00Z" }),
    );
    expect(peaks.list("test-model", sha)).toEqual([
      record({ peakBytes: 3 * 1024 ** 3 }),
    ]);
    peaks.record(
      record({ peakBytes: 4 * 1024 ** 3, observedAt: "2026-10-17T00:00:00Z" }),
    );
    expect(peaks.list("test-model", sha)[0]?.peakBytes).toBe(4 * 1024 ** 3);
  });

  test("lists every launch shape for the artifact, newest first", () => {
    const peaks = store();
    peaks.record(record({ launchShape: "a", observedAt: "2026-10-01T00:00Z" }));
    peaks.record(record({ launchShape: "b", observedAt: "2026-10-02T00:00Z" }));
    peaks.record(record({ artifactSha256: "b".repeat(64), launchShape: "c" }));
    expect(
      peaks.list("test-model", sha).map((entry) => entry.launchShape),
    ).toEqual(["b", "a"]);
  });
});

describe("observation targets", () => {
  const llm = byId("qwen2.5-coder-1.5b-instruct-q4_k_m")!;
  const plan = {
    modality: "llm",
    modelFile: primaryArtifact(llm).filename,
    ctxSize: 8192,
    parallel: { slots: 2 },
    kvCache: { typeK: "q8_0", typeV: "q8_0" },
  } as unknown as RuntimeLaunchPlan;

  test("LLM shapes key on context, slots, and KV cache type", () => {
    expect(planLaunchShape(plan)).toBe(
      llmLaunchShape({ ctxSize: 8192, slots: 2, typeK: "q8_0", typeV: "q8_0" }),
    );
    expect(planLaunchShape({ modality: "stt" } as RuntimeLaunchPlan)).toBe(
      DEFAULT_LAUNCH_SHAPE,
    );
  });

  test("records only launches of the pinned catalog artifact", () => {
    expect(observationTarget(llm, plan)).toEqual({
      modelId: llm.modelId,
      artifactSha256: primaryArtifact(llm).sha256!,
      launchShape: "ctx=8192;slots=2;kv=q8_0/q8_0",
    });
    expect(observationTarget(undefined, plan)).toBeNull();
    expect(
      observationTarget(llm, {
        ...plan,
        modelFile: "override.gguf",
      } as RuntimeLaunchPlan),
    ).toBeNull();
    expect(
      observationTarget(llm, { ...plan, modality: "stt" } as RuntimeLaunchPlan),
    ).toBeNull();
  });
});

describe("observePeakMemory", () => {
  test("persists each new peak until the backend exits", async () => {
    const peaks = store();
    const samples: (MemorySample | null)[] = [
      { bytes: 100, method: "footprint" },
      null,
      { bytes: 300, method: "footprint" },
      { bytes: 200, method: "footprint" },
    ];
    let exit!: () => void;
    const exited = new Promise<void>((resolve) => (exit = resolve));
    const recorded: number[] = [];
    const target = {
      modelId: "test-model",
      artifactSha256: sha,
      launchShape: DEFAULT_LAUNCH_SHAPE,
    };
    const peak = await observePeakMemory({
      pid: 1,
      target,
      exited,
      intervalMs: 1,
      sampler: async () => {
        const sample = samples.shift();
        if (samples.length === 0) exit();
        return sample ?? null;
      },
      store: {
        record: (entry) => {
          recorded.push(entry.peakBytes);
          peaks.record(entry);
        },
        list: peaks.list,
      },
    });
    expect(peak).toBe(300);
    expect(recorded).toEqual([100, 300]);
    expect(peaks.list("test-model", sha)[0]?.peakBytes).toBe(300);
  });

  test("stops when aborted, passing the signal to the sampler", async () => {
    const controller = new AbortController();
    const signals: (AbortSignal | undefined)[] = [];
    const started = Date.now();
    const peak = await observePeakMemory({
      pid: 1,
      target: {
        modelId: "m",
        artifactSha256: sha,
        launchShape: DEFAULT_LAUNCH_SHAPE,
      },
      exited: new Promise(() => {}),
      intervalMs: 60_000,
      signal: controller.signal,
      sampler: async (_pid, signal) => {
        signals.push(signal);
        setTimeout(() => controller.abort(), 1);
        return { bytes: 7, method: "footprint" };
      },
      store: { record: () => {}, list: () => [] },
    });
    expect(peak).toBe(7);
    expect(signals).toEqual([controller.signal]);
    expect(Date.now() - started).toBeLessThan(5_000);
  });

  test("reports sampler errors and keeps sampling", async () => {
    let calls = 0;
    const errors: unknown[] = [];
    let exit!: () => void;
    const exited = new Promise<void>((resolve) => (exit = resolve));
    const peak = await observePeakMemory({
      pid: 1,
      target: {
        modelId: "m",
        artifactSha256: sha,
        launchShape: DEFAULT_LAUNCH_SHAPE,
      },
      exited,
      intervalMs: 1,
      sampler: async () => {
        calls += 1;
        if (calls === 1) throw new Error("boom");
        exit();
        return { bytes: 5, method: "vmhwm" };
      },
      store: { record: () => {}, list: () => [] },
      onError: (error) => errors.push(error),
    });
    expect(errors).toHaveLength(1);
    expect(calls).toBe(2);
    // The sample that raced the exit is discarded.
    expect(peak).toBe(0);
  });
});
