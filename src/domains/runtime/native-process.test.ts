import { expect, test } from "bun:test";
import { NativeProcessStopError, stopNativeProcess } from "./native-process";

function controlledChild() {
  const exit = Promise.withResolvers<number>();
  const killed = Promise.withResolvers<void>();
  const signals: Array<number | NodeJS.Signals | undefined> = [];
  const child = {
    pid: 42424,
    exitCode: null,
    signalCode: null,
    exited: exit.promise,
    kill(signal?: number | NodeJS.Signals) {
      signals.push(signal);
      if (signal === "SIGKILL") killed.resolve();
    },
  } satisfies Parameters<typeof stopNativeProcess>[0];
  return { child, exit, killed, signals };
}

test("waits for owned exit after escalation without another confirmation deadline", async () => {
  const { child, exit, killed, signals } = controlledChild();
  const waits: number[] = [];
  let settled = false;
  const stopping = stopNativeProcess(child, 500, async (durationMs) => {
    waits.push(durationMs);
    if (signals.includes("SIGKILL")) {
      throw new Error("Unexpected post-KILL confirmation deadline.");
    }
  }).finally(() => {
    settled = true;
  });
  try {
    await killed.promise;
    expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
    expect(settled).toBe(false);
    expect(waits).toEqual([0, 500]);
  } finally {
    exit.resolve(0);
  }
  await stopping;
  expect(settled).toBe(true);
});

test("reports signal failures without claiming an owned child exited", async () => {
  const { child, exit, signals } = controlledChild();
  const denied = new Error("signal denied");
  child.kill = (signal) => {
    signals.push(signal);
    throw denied;
  };
  try {
    await expect(
      stopNativeProcess(child, 500, async () => {}),
    ).rejects.toBeInstanceOf(NativeProcessStopError);
    expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
  } finally {
    exit.resolve(0);
  }
});

test("reports exit observation failure after escalation", async () => {
  const { child, exit, killed } = controlledChild();
  const stopping = stopNativeProcess(child, 500, async () => {});
  const rejection = stopping.then(
    () => {
      throw new Error("Expected exit observation failure.");
    },
    (error: unknown) => expect(error).toBeInstanceOf(NativeProcessStopError),
  );
  await killed.promise;
  exit.reject(new Error("exit observation failed"));
  await rejection;
});
