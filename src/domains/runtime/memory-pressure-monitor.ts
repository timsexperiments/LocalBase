import type { MemorySafetyController } from "./memory-controller";
import type { MemorySafetyTransition } from "./memory-safety";

export const memoryPressurePollIntervalMs = 2_000;

export type MemoryPressureMonitorOptions = Readonly<{
  controller: Pick<MemorySafetyController, "poll">;
  onElevatedPressure: (
    transition: MemorySafetyTransition,
  ) => Promise<void> | void;
  onTransition: (transition: MemorySafetyTransition) => Promise<void> | void;
  onError: (error: unknown) => Promise<void> | void;
  intervalMs?: number;
}>;

/** Polls host memory and reports state changes without overlapping samples. */
export class MemoryPressureMonitor {
  private interval: ReturnType<typeof setInterval> | undefined;
  private running: Promise<void> | undefined;
  private reportedError = false;
  private lastApplied: MemorySafetyTransition["current"] = {
    state: "healthy",
    consecutiveNormalSnapshots: 0,
  };

  constructor(private readonly options: MemoryPressureMonitorOptions) {}

  start(): void {
    if (this.interval) return;
    void this.poll().catch(() => {});
    this.interval = setInterval(() => {
      void this.poll().catch(() => {});
    }, this.options.intervalMs ?? memoryPressurePollIntervalMs);
  }

  async stop(): Promise<void> {
    if (this.interval) clearInterval(this.interval);
    this.interval = undefined;
    await this.running?.catch(() => {});
  }

  async poll(): Promise<void> {
    if (this.running) return await this.running;
    const running = this.observe();
    this.running = running;
    try {
      await running;
    } finally {
      if (this.running === running) this.running = undefined;
    }
  }

  private async observe(): Promise<void> {
    try {
      const transition = await this.options.controller.poll();
      const stateChanged = this.lastApplied.state !== transition.current.state;
      const appliedTransition = {
        previous: this.lastApplied,
        current: transition.current,
        action: transition.action,
      } satisfies MemorySafetyTransition;
      if (
        transition.current.state === "constrained" ||
        (transition.current.state === "critical" && stateChanged)
      ) {
        await this.options.onElevatedPressure(appliedTransition);
      }
      if (stateChanged) {
        this.lastApplied = transition.current;
        await this.options.onTransition(appliedTransition);
      }
      this.reportedError = false;
    } catch (error) {
      if (!this.reportedError) {
        this.reportedError = true;
        try {
          await this.options.onError(error);
        } catch {}
      }
      throw error;
    }
  }
}
