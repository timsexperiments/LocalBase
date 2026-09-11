import { expect, test } from "bun:test";
import {
  InferenceQueue,
  InferenceQueueAbortedError,
  InferenceQueueCapacityError,
  InferenceQueueTimeoutError,
  InferenceQueueUnavailableError,
} from "./inference-queue";

type Result = Readonly<{ admitted: boolean; slots: number; id: string }>;

function queue(
  modality: "llm" | "stt" | "image" = "llm",
  options: Readonly<{ maxWaiting?: number; waitMs?: number }> = {},
) {
  return new InferenceQueue<Result>(modality, {
    ...options,
    isAdmitted: ({ admitted }) => admitted,
    resolvedSlots: ({ slots }) => slots,
  });
}

test("wakes same-model waiters when a cold LLM resolves more slots", async () => {
  let slots = 1;
  let markReady!: () => void;
  const ready = new Promise<void>((resolve) => {
    markReady = resolve;
  });
  const inferenceQueue = new InferenceQueue<Result>("llm", {
    isAdmitted: ({ admitted }) => admitted,
    resolvedSlots: () => slots,
    whenSlotsResolved: async () => await ready,
  });
  const first = await inferenceQueue.acquire("a", async () => ({
    admitted: true,
    slots: 1,
    id: "first",
  }));
  let secondDispatched = false;
  const second = inferenceQueue.acquire("a", async () => {
    secondDispatched = true;
    return { admitted: true, slots: 3, id: "second" };
  });
  await Promise.resolve();
  expect(secondDispatched).toBe(false);
  slots = 3;
  markReady();
  const secondLease = await second;
  expect(secondDispatched).toBe(true);
  expect(inferenceQueue.snapshot().active).toBe(2);
  first.release();
  secondLease.release();
});

test("dispatches FIFO and holds permits until exactly-once release", async () => {
  const inferenceQueue = queue("llm");
  const order: string[] = [];
  const first = await inferenceQueue.acquire("a", async () => {
    order.push("first");
    return { admitted: true, slots: 2, id: "first" };
  });
  const second = inferenceQueue.acquire("a", async () => {
    order.push("second");
    return { admitted: true, slots: 2, id: "second" };
  });
  const third = inferenceQueue.acquire("a", async () => {
    order.push("third");
    return { admitted: true, slots: 2, id: "third" };
  });
  const secondLease = await second;
  expect(order).toEqual(["first", "second"]);
  expect(inferenceQueue.snapshot()).toMatchObject({ active: 2, waiting: 1 });
  first.release();
  first.release();
  const thirdLease = await third;
  expect(order).toEqual(["first", "second", "third"]);
  secondLease.release();
  thirdLease.release();
  expect(inferenceQueue.snapshot()).toMatchObject({ active: 0, waiting: 0 });
});

test("uses one authoritative permit for STT and image", async () => {
  for (const modality of ["stt", "image"] as const) {
    const inferenceQueue = queue(modality);
    const first = await inferenceQueue.acquire("model", async () => ({
      admitted: true,
      slots: 8,
      id: "first",
    }));
    let dispatched = false;
    const second = inferenceQueue.acquire("model", async () => {
      dispatched = true;
      return { admitted: true, slots: 8, id: "second" };
    });
    await Promise.resolve();
    expect(dispatched).toBe(false);
    first.release();
    (await second).release();
  }
});

test("bounds waiting work with captured capacity and deadline", async () => {
  const inferenceQueue = queue("llm", { maxWaiting: 1, waitMs: 20 });
  const first = await inferenceQueue.acquire("a", async () => ({
    admitted: true,
    slots: 1,
    id: "first",
  }));
  const timedOut = inferenceQueue.acquire("a", async () => ({
    admitted: true,
    slots: 1,
    id: "timeout",
  }));
  await expect(
    inferenceQueue.acquire("a", async () => ({
      admitted: true,
      slots: 1,
      id: "overflow",
    })),
  ).rejects.toBeInstanceOf(InferenceQueueCapacityError);
  expect(inferenceQueue.snapshot()).toEqual({
    waiting: 1,
    active: 1,
    capacity: 1,
    maxWaitMs: 20,
  });
  await expect(timedOut).rejects.toBeInstanceOf(InferenceQueueTimeoutError);
  first.release();
});

test("removes an aborted waiter without dispatching it", async () => {
  const inferenceQueue = queue();
  const first = await inferenceQueue.acquire("a", async () => ({
    admitted: true,
    slots: 1,
    id: "first",
  }));
  const abort = new AbortController();
  let dispatched = false;
  const waiting = inferenceQueue.acquire(
    "a",
    async () => {
      dispatched = true;
      return { admitted: true, slots: 1, id: "aborted" };
    },
    abort.signal,
  );
  abort.abort();
  await expect(waiting).rejects.toBeInstanceOf(InferenceQueueAbortedError);
  first.release();
  await Promise.resolve();
  expect(dispatched).toBe(false);
});

test("keeps a model switch queued and preserves FIFO behind it", async () => {
  const inferenceQueue = queue();
  const first = await inferenceQueue.acquire("a", async () => ({
    admitted: true,
    slots: 2,
    id: "first",
  }));
  const abort = new AbortController();
  let switchDispatched = false;
  const switching = inferenceQueue.acquire(
    "b",
    async () => {
      switchDispatched = true;
      return { admitted: true, slots: 2, id: "switch" };
    },
    abort.signal,
  );
  let laterDispatched = false;
  const laterA = inferenceQueue.acquire("a", async () => {
    laterDispatched = true;
    return { admitted: true, slots: 2, id: "later" };
  });
  await Promise.resolve();
  expect(switchDispatched).toBe(false);
  expect(laterDispatched).toBe(false);
  abort.abort();
  await expect(switching).rejects.toBeInstanceOf(InferenceQueueAbortedError);
  const laterLease = await laterA;
  expect(laterDispatched).toBe(true);
  expect(switchDispatched).toBe(false);
  first.release();
  laterLease.release();
});

test("measures queue wait at dispatch rather than after activation", async () => {
  let now = 10;
  let finishDispatch!: () => void;
  const dispatchBlocked = new Promise<void>((resolve) => {
    finishDispatch = resolve;
  });
  const inferenceQueue = new InferenceQueue<Result>("llm", {
    isAdmitted: ({ admitted }) => admitted,
    resolvedSlots: ({ slots }) => slots,
    now: () => now,
  });
  const admission = inferenceQueue.acquire("a", async () => {
    await dispatchBlocked;
    return { admitted: true, slots: 1, id: "activated" };
  });
  await Promise.resolve();
  now = 500;
  finishDispatch();
  const lease = await admission;
  expect(lease.queueWaitMs).toBe(0);
  lease.release();
});

test("rejects pending work on disable, emergency, and shutdown", async () => {
  for (const close of [false, true]) {
    const inferenceQueue = queue();
    const first = await inferenceQueue.acquire("a", async () => ({
      admitted: true,
      slots: 1,
      id: "first",
    }));
    const waiting = inferenceQueue.acquire("a", async () => ({
      admitted: true,
      slots: 1,
      id: "waiting",
    }));
    const error = new InferenceQueueUnavailableError(
      close ? "shutdown" : "disabled",
    );
    close ? inferenceQueue.close(error) : inferenceQueue.rejectPending(error);
    await expect(waiting).rejects.toBe(error);
    if (close) {
      await expect(
        inferenceQueue.acquire("a", async () => ({
          admitted: true,
          slots: 1,
          id: "late",
        })),
      ).rejects.toBe(error);
    }
    first.release();
  }
});

test("resets model ownership after dispatch failure", async () => {
  const inferenceQueue = queue();
  await expect(
    inferenceQueue.acquire("a", async () => {
      throw new Error("dispatch failed");
    }),
  ).rejects.toThrow("dispatch failed");
  const recovered = await inferenceQueue.acquire("b", async () => ({
    admitted: true,
    slots: 1,
    id: "recovered",
  }));
  expect(recovered.value.id).toBe("recovered");
  recovered.release();
});
