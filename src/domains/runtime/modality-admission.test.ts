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
  await Bun.sleep(0);
  expect(drained).toBe(false);
  response.release();
  await drain;

  barrier.attach();
  expect(barrier.acquire("replacement")?.value).toBe("replacement");
});
