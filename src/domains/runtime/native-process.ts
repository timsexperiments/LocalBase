export class NativeProcessStopError extends Error {
  constructor(pid: number, causes: readonly unknown[] = []) {
    super(`Native process ${pid} could not be confirmed stopped.`, {
      cause:
        causes.length > 0
          ? new AggregateError(causes, "Native process termination failed.")
          : undefined,
    });
    this.name = "NativeProcessStopError";
  }
}

type NativeChild = Pick<
  Bun.Subprocess,
  "pid" | "kill" | "exited" | "exitCode" | "signalCode"
>;

function hasExited(child: NativeChild): boolean {
  return typeof child.exitCode === "number" || child.signalCode != null;
}

/** Resolves only after a native child exit is confirmed. */
export async function stopNativeProcess(
  child: NativeChild,
  graceMs: number,
  waitForGrace: (durationMs: number) => Promise<void> = Bun.sleep,
): Promise<void> {
  const errors: unknown[] = [];
  const exitedWithin = async (durationMs: number): Promise<boolean> => {
    if (hasExited(child)) return true;
    return await Promise.race([
      child.exited.then(
        () => true,
        (error: unknown) => {
          errors.push(error);
          return false;
        },
      ),
      waitForGrace(durationMs).then(() => false),
    ]);
  };

  if (await exitedWithin(0)) return;
  try {
    child.kill("SIGTERM");
  } catch (error) {
    errors.push(error);
  }
  if (await exitedWithin(graceMs)) return;
  try {
    child.kill("SIGKILL");
  } catch (error) {
    errors.push(error);
    if (hasExited(child)) return;
    throw new NativeProcessStopError(child.pid, errors);
  }
  // Signal delivery does not confirm exit; retain ownership through native cleanup.
  try {
    await child.exited;
  } catch (error) {
    errors.push(error);
    throw new NativeProcessStopError(child.pid, errors);
  }
}
