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
