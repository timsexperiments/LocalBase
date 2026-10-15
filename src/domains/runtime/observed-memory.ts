import { and, eq } from "drizzle-orm";
import { primaryArtifact, type ModelSpec } from "../../catalog";
import type { LocalBaseDatabase } from "../../db/client";
import { modelObservedMemoryTable } from "../../db/schema";
import type { RuntimeLaunchPlan } from "./launch-plan";

/**
 * How a peak was read.
 * - `footprint`: macOS `footprint -p` TOTAL dirty + clean pages. On unified
 *   memory this covers Metal buffers and mapped weights, so it is the whole
 *   demand of the process.
 * - `vmhwm`: Linux `/proc/<pid>/status` VmHWM, the host RSS high-water mark.
 *   It does not include discrete GPU memory.
 */
export type ObservedMemoryMethod = "footprint" | "vmhwm";

/** The highest memory one model reached on this machine at one launch shape. */
export type ObservedMemoryRecord = Readonly<{
  modelId: string;
  artifactSha256: string;
  launchShape: string;
  peakBytes: number;
  method: ObservedMemoryMethod;
  observedAt: string;
}>;

export interface ObservedMemoryStore {
  /** Keeps the larger of the stored and the new peak. */
  record(record: ObservedMemoryRecord): void;
  /** Every launch shape recorded for one artifact, newest first. */
  list(modelId: string, artifactSha256: string): ObservedMemoryRecord[];
}

function decodeMethod(text: string): ObservedMemoryMethod {
  return text === "vmhwm" ? "vmhwm" : "footprint";
}

export function createObservedMemoryStore(
  database: LocalBaseDatabase,
): ObservedMemoryStore {
  const table = modelObservedMemoryTable;
  return {
    record(record) {
      const existing = database
        .select()
        .from(table)
        .where(
          and(
            eq(table.modelId, record.modelId),
            eq(table.artifactSha256, record.artifactSha256),
            eq(table.launchShape, record.launchShape),
          ),
        )
        .get();
      if (existing && existing.peakBytes >= record.peakBytes) return;
      const values = {
        modelId: record.modelId,
        artifactSha256: record.artifactSha256,
        launchShape: record.launchShape,
        peakBytes: Math.round(record.peakBytes),
        method: record.method,
        observedAt: record.observedAt,
      };
      database
        .insert(table)
        .values(values)
        .onConflictDoUpdate({
          target: [table.modelId, table.artifactSha256, table.launchShape],
          set: {
            peakBytes: values.peakBytes,
            method: values.method,
            observedAt: values.observedAt,
          },
        })
        .run();
    },
    list(modelId, artifactSha256) {
      return database
        .select()
        .from(table)
        .where(
          and(
            eq(table.modelId, modelId),
            eq(table.artifactSha256, artifactSha256),
          ),
        )
        .all()
        .map((row) => ({
          modelId: row.modelId,
          artifactSha256: row.artifactSha256,
          launchShape: row.launchShape,
          peakBytes: row.peakBytes,
          method: decodeMethod(row.method),
          observedAt: row.observedAt,
        }))
        .sort((a, b) => b.observedAt.localeCompare(a.observedAt));
    },
  };
}

/** Shape used for runtimes whose memory does not depend on launch settings. */
export const DEFAULT_LAUNCH_SHAPE = "default";

export function llmLaunchShape(
  input: Readonly<{
    ctxSize: number;
    slots: number;
    typeK: string;
    typeV: string;
  }>,
): string {
  return `ctx=${input.ctxSize};slots=${input.slots};kv=${input.typeK}/${input.typeV}`;
}

/** The launch-shape key for a plan; only LLM memory varies by launch settings here. */
export function planLaunchShape(plan: RuntimeLaunchPlan): string {
  if (plan.modality !== "llm") return DEFAULT_LAUNCH_SHAPE;
  return llmLaunchShape({
    ctxSize: plan.ctxSize,
    slots: plan.parallel.slots,
    typeK: plan.kvCache.typeK,
    typeV: plan.kvCache.typeV,
  });
}

export type ObservationTarget = Readonly<{
  modelId: string;
  artifactSha256: string;
  launchShape: string;
}>;

/**
 * Peaks are keyed to the catalog artifact, so a launch that loads some other
 * file (a model-file override or an unpinned model) is not recorded.
 */
export function observationTarget(
  spec: ModelSpec | undefined,
  plan: RuntimeLaunchPlan,
): ObservationTarget | null {
  if (!spec || spec.kind !== plan.modality) return null;
  const primary = primaryArtifact(spec);
  if (!primary.sha256) return null;
  if ("modelFile" in plan && plan.modelFile !== primary.filename) return null;
  return {
    modelId: spec.modelId,
    artifactSha256: primary.sha256,
    launchShape: planLaunchShape(plan),
  };
}

const sizeUnits: Readonly<Record<string, number>> = {
  B: 1,
  KB: 1024,
  MB: 1024 ** 2,
  GB: 1024 ** 3,
  TB: 1024 ** 4,
};

function parseSize(value: string, unit: string): number | null {
  const scale = sizeUnits[unit.toUpperCase()];
  const amount = Number(value);
  if (scale === undefined || !Number.isFinite(amount)) return null;
  return amount * scale;
}

/**
 * Bytes a process holds per macOS `footprint`: dirty plus clean pages from
 * the TOTAL row. `phys_footprint` alone omits clean mapped weights, which
 * are resident while a model serves.
 */
export function parseFootprintTotal(output: string): number | null {
  for (const line of output.split("\n")) {
    if (!/\bTOTAL\s*$/.test(line)) continue;
    const sizes = [...line.matchAll(/([\d.]+)\s*(B|KB|MB|GB|TB)\b/gi)];
    if (sizes.length < 2) return null;
    const dirty = parseSize(sizes[0]![1]!, sizes[0]![2]!);
    const clean = parseSize(sizes[1]![1]!, sizes[1]![2]!);
    if (dirty === null || clean === null) return null;
    return Math.round(dirty + clean);
  }
  return null;
}

/** VmHWM from `/proc/<pid>/status`, in bytes. */
export function parseVmHwm(status: string): number | null {
  const match = /^VmHWM:\s+(\d+)\s*kB/im.exec(status);
  return match ? Number(match[1]) * 1024 : null;
}

export type MemorySample = Readonly<{
  bytes: number;
  method: ObservedMemoryMethod;
}>;

export type MemorySampler = (
  pid: number,
  signal?: AbortSignal,
) => Promise<MemorySample | null>;

/** A `footprint` run that takes longer than this is killed and skipped. */
export const SAMPLE_TIMEOUT_MS = 2_000;

/** Reads one process's memory with the platform's tool; null when unavailable. */
export function platformMemorySampler(
  platform: NodeJS.Platform = process.platform,
): MemorySampler | null {
  if (platform === "darwin") {
    return async (pid, signal) => {
      if (signal?.aborted) return null;
      try {
        // footprint can stall on a process that is exiting; the timeout and
        // the abort signal keep a stuck sample from holding the gateway open.
        const proc = Bun.spawn(["/usr/bin/footprint", "-p", String(pid)], {
          stdout: "pipe",
          stderr: "ignore",
          stdin: "ignore",
          timeout: SAMPLE_TIMEOUT_MS,
          killSignal: "SIGKILL",
          ...(signal ? { signal } : {}),
        });
        const [output, code] = await Promise.all([
          new Response(proc.stdout).text(),
          proc.exited,
        ]);
        if (code !== 0) return null;
        const bytes = parseFootprintTotal(output);
        return bytes === null ? null : { bytes, method: "footprint" };
      } catch {
        return null;
      }
    };
  }
  if (platform === "linux") {
    return async (pid) => {
      try {
        const bytes = parseVmHwm(await Bun.file(`/proc/${pid}/status`).text());
        return bytes === null ? null : { bytes, method: "vmhwm" };
      } catch {
        return null;
      }
    };
  }
  return null;
}

export const OBSERVATION_INTERVAL_MS = 5_000;

export type PeakObservation = Readonly<{
  pid: number;
  target: ObservationTarget;
  exited: Promise<unknown>;
  sampler: MemorySampler;
  store: ObservedMemoryStore;
  intervalMs?: number;
  now?: () => Date;
  onError?: (error: unknown) => void;
  /** Stops sampling, for example when the gateway shuts down. */
  signal?: AbortSignal;
}>;

/**
 * Samples a serving backend until it exits and persists each new peak. Only
 * increases are written, so steady state costs one sample per interval.
 */
export async function observePeakMemory(
  observation: PeakObservation,
): Promise<number> {
  const intervalMs = observation.intervalMs ?? OBSERVATION_INTERVAL_MS;
  const now = observation.now ?? (() => new Date());
  const signal = observation.signal;
  let exited = signal?.aborted ?? false;
  const stopped = new Promise<void>((resolve) => {
    const stop = () => {
      exited = true;
      resolve();
    };
    void observation.exited.then(stop, stop);
    signal?.addEventListener("abort", stop, { once: true });
  });
  let peak = 0;
  while (!exited) {
    try {
      const sample = await observation.sampler(observation.pid, signal);
      if (exited) break;
      if (sample && sample.bytes > peak) {
        peak = sample.bytes;
        observation.store.record({
          ...observation.target,
          peakBytes: sample.bytes,
          method: sample.method,
          observedAt: now().toISOString(),
        });
      }
    } catch (error) {
      observation.onError?.(error);
    }
    if (exited) break;
    // A plain timer, cleared on exit, so a stopped observation never keeps
    // the event loop alive for the rest of an interval.
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      stopped,
      new Promise<void>((resolve) => {
        timer = setTimeout(resolve, intervalMs);
      }),
    ]);
    clearTimeout(timer);
  }
  return peak;
}
