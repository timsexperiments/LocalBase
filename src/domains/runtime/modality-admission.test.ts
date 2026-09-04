import { expect, test } from "bun:test";
import { ModalityAdmissionBarrier } from "./modality-admission";

test("detaches new admission, cancels one pending startup, and drains active responses", async () => {
  const barrier = new ModalityAdmissionBarrier("stt", true);
  const startup = barrier.acquire("startup");
  const waitingStartup = barrier.acquire("waiting-startup");
  const response = barrier.acquire("response");
  if (!startup || !waitingStartup || !response) {
    throw new Error("Expected admissions.");
  }

  let startupCancelled = 0;
  let responseCancelled = 0;
  startup.onPendingDetach(() => {
    startupCancelled += 1;
  });
  waitingStartup.onPendingDetach(() => {
    startupCancelled += 1;
  });
  response.onPendingDetach(() => {
    responseCancelled += 1;
  });
  response.markResponseStarted();

  let drained = false;
  const drain = barrier.drain().then(() => {
    drained = true;
  });
  expect(startupCancelled).toBe(1);
  expect(responseCancelled).toBe(0);
  expect(barrier.acquire("new")).toBeUndefined();

  startup.cancel();
  waitingStartup.release();
  await Promise.resolve();
  expect(drained).toBe(false);
  response.release();
  await drain;
  expect(startupCancelled).toBe(1);

  barrier.attach();
  expect(barrier.acquire("replacement")?.value).toBe("replacement");
});

test("detaches atomically only when no requests are admitted", () => {
  const barrier = new ModalityAdmissionBarrier("llm", true);

  const admission = barrier.acquire("active");
  expect(admission).toBeDefined();
  expect(barrier.detachIfIdle()).toBe(false);

  admission!.release();
  expect(barrier.detachIfIdle()).toBe(true);
  expect(barrier.acquire("rejected")).toBeUndefined();

  barrier.attach();
  expect(barrier.acquire("accepted")?.value).toBe("accepted");
});

test("cancels an orphaned runtime after all shared admissions settle", () => {
  const barrier = new ModalityAdmissionBarrier("llm", true);
  const cancelled = barrier.acquire("cancelled");
  const completed = barrier.acquire("completed");
  if (!cancelled || !completed) throw new Error("Expected admissions.");

  let cancellations = 0;
  cancelled.onIdleCancellation(() => {
    cancellations += 1;
  });
  completed.onIdleCancellation(() => {
    cancellations += 1;
  });

  cancelled.cancel();
  expect(cancellations).toBe(0);

  completed.release();
  expect(cancellations).toBe(1);
});

test("cancels immediately when the final admission is cancelled", () => {
  const barrier = new ModalityAdmissionBarrier("image", true);
  const admission = barrier.acquire("cancelled");
  if (!admission) throw new Error("Expected admission.");
  let cancellations = 0;
  admission.onIdleCancellation(() => {
    cancellations += 1;
  });

  admission.cancel();

  expect(cancellations).toBe(1);
});

test("drains leases without invoking pending cancellation", async () => {
  const barrier = new ModalityAdmissionBarrier("image", true);
  const admission = barrier.acquire("pending");
  if (!admission) throw new Error("Expected admission.");
  let cancellations = 0;
  admission.onPendingDetach(() => {
    cancellations += 1;
  });

  const drain = barrier.drainWithoutCancellation();
  expect(cancellations).toBe(0);
  expect(barrier.acquire("rejected")).toBeUndefined();

  admission.release();
  await drain;
});
