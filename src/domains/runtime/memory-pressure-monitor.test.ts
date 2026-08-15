import { describe, expect, test } from "bun:test";
import { MemoryPressureMonitor } from "./memory-pressure-monitor";
import type { MemorySafetyTransition } from "./memory-safety";

function transition(
  previous: MemorySafetyTransition["previous"]["state"],
  current: MemorySafetyTransition["current"]["state"],
): MemorySafetyTransition {
  return {
    previous: { state: previous, consecutiveNormalSnapshots: 0 },
    current: { state: current, consecutiveNormalSnapshots: 0 },
    action:
      current === "critical"
        ? "emergency-stop"
        : current === "constrained"
          ? "constrain"
          : "allow",
  };
}

function controller(
  results: readonly (MemorySafetyTransition | Error)[],
): Pick<{ poll: () => Promise<MemorySafetyTransition> }, "poll"> {
  let index = 0;
  return {
    async poll() {
      const result = results[Math.min(index, results.length - 1)]!;
      index += 1;
      if (result instanceof Error) throw result;
      return result;
    },
  };
}

describe("memory pressure monitor", () => {
  test("acts once for each state transition", async () => {
    const observed: string[] = [];
    const monitor = new MemoryPressureMonitor({
      controller: controller([
        transition("healthy", "constrained"),
        transition("constrained", "constrained"),
        transition("constrained", "critical"),
        transition("critical", "critical"),
        transition("critical", "healthy"),
        transition("healthy", "critical"),
      ]),
      onTransition: ({ current }) => {
        observed.push(current.state);
      },
      onError: () => {},
    });

    for (let index = 0; index < 6; index += 1) await monitor.poll();

    expect(observed).toEqual([
      "constrained",
      "critical",
      "healthy",
      "critical",
    ]);
  });

  test("serializes concurrent polls", async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let polls = 0;
    const monitor = new MemoryPressureMonitor({
      controller: {
        async poll() {
          polls += 1;
          await gate;
          return transition("healthy", "healthy");
        },
      },
      onTransition: () => {},
      onError: () => {},
    });

    const first = monitor.poll();
    const second = monitor.poll();
    expect(polls).toBe(1);
    release?.();
    await Promise.all([first, second]);
    expect(polls).toBe(1);
  });

  test("reports a monitor error once until a successful poll", async () => {
    const errors: string[] = [];
    const monitor = new MemoryPressureMonitor({
      controller: controller([
        new Error("unavailable"),
        new Error("unavailable"),
        transition("healthy", "healthy"),
        new Error("unavailable"),
      ]),
      onTransition: () => {},
      onError: (error) => {
        errors.push((error as Error).message);
      },
    });

    await expect(monitor.poll()).rejects.toThrow("unavailable");
    await expect(monitor.poll()).rejects.toThrow("unavailable");
    await monitor.poll();
    await expect(monitor.poll()).rejects.toThrow("unavailable");

    expect(errors).toEqual(["unavailable", "unavailable"]);
  });

  test("retries a failed critical transition from the last applied state", async () => {
    const errors: string[] = [];
    const transitions: Array<{
      previous: string;
      current: string;
    }> = [];
    const successfulActions: string[] = [];
    let attempts = 0;
    const monitor = new MemoryPressureMonitor({
      controller: controller([
        transition("healthy", "critical"),
        transition("critical", "critical"),
      ]),
      onTransition: ({ previous, current }) => {
        transitions.push({ previous: previous.state, current: current.state });
        attempts += 1;
        if (attempts === 1) throw new Error("eviction failed");
        successfulActions.push(current.state);
      },
      onError: (error) => {
        errors.push((error as Error).message);
      },
    });

    await expect(monitor.poll()).rejects.toThrow("eviction failed");
    await monitor.poll();

    expect(transitions).toEqual([
      { previous: "healthy", current: "critical" },
      { previous: "healthy", current: "critical" },
    ]);
    expect(successfulActions).toEqual(["critical"]);
    expect(errors).toEqual(["eviction failed"]);
  });

  test("polls immediately when started", async () => {
    let polls = 0;
    const monitor = new MemoryPressureMonitor({
      controller: {
        async poll() {
          polls += 1;
          return transition("healthy", "healthy");
        },
      },
      onTransition: () => {},
      onError: () => {},
    });

    monitor.start();
    await monitor.stop();

    expect(polls).toBe(1);
  });
});
