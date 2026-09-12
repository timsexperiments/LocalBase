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

function hasExited(child: Bun.Subprocess): boolean {
  return typeof child.exitCode === "number" || child.signalCode != null;
}

/** Resolves only after a native child exit is confirmed. */
export async function stopNativeProcess(
  child: Bun.Subprocess,
  graceMs: number,
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
      Bun.sleep(durationMs).then(() => false),
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
  }
  if (await exitedWithin(graceMs)) return;
  throw new NativeProcessStopError(child.pid, errors);
}
