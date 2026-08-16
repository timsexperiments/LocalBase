import { expect, test } from "bun:test";
import { ModalityAdmissionBarrier } from "./modality-admission";

test("detaches new admission, cancels startup work, and drains active responses", async () => {
  const barrier = new ModalityAdmissionBarrier("stt", true);
  const startup = barrier.acquire("startup");
  const response = barrier.acquire("response");
  if (!startup || !response) throw new Error("Expected admissions.");

  let startupCancelled = 0;
  let responseCancelled = 0;
  startup.onDetach(() => {
    startupCancelled += 1;
  });
  response.onDetach(() => {
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

  startup.release();
  await Promise.resolve();
  expect(drained).toBe(false);
  response.release();
  await drain;

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

test("drains leases without invoking pending cancellation", async () => {
  const barrier = new ModalityAdmissionBarrier("image", true);
  const admission = barrier.acquire("pending");
  if (!admission) throw new Error("Expected admission.");
  let cancellations = 0;
  admission.onDetach(() => {
    cancellations += 1;
  });

  const drain = barrier.drainWithoutCancellation();
  expect(cancellations).toBe(0);
  expect(barrier.acquire("rejected")).toBeUndefined();

  admission.release();
  await drain;
});
