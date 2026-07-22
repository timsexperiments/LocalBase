import { z } from "zod";

export const BACKEND_GUARDIAN_COMMAND = "__localbase_backend_guardian";

const guardianArgumentsSchema = z.tuple([
  z.coerce.number().int().positive(),
  z.coerce.number().int().positive(),
]);

const POLL_INTERVAL_MS = 100;
const BACKEND_STOP_GRACE_MS = 500;

function isRunning(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
}

async function stopBackend(pid: number): Promise<void> {
  if (!isRunning(pid)) return;
  try {
    process.kill(pid, "SIGTERM");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return;
    throw error;
  }

  const deadline = Date.now() + BACKEND_STOP_GRACE_MS;
  while (Date.now() < deadline) {
    if (!isRunning(pid)) return;
    await Bun.sleep(POLL_INTERVAL_MS);
  }
  if (isRunning(pid)) process.kill(pid, "SIGKILL");
}

/** Reaps a backend if its gateway exits without running normal shutdown. */
export async function runBackendGuardian(args: string[]): Promise<number> {
  const parsed = guardianArgumentsSchema.safeParse(args);
  if (!parsed.success) return 2;

  const [gatewayPid, backendPid] = parsed.data;
  if (gatewayPid === process.pid || backendPid === process.pid) return 2;
  if (!isRunning(gatewayPid) || !isRunning(backendPid)) return 0;

  while (isRunning(gatewayPid) && isRunning(backendPid)) {
    await Bun.sleep(POLL_INTERVAL_MS);
  }
  if (!isRunning(gatewayPid)) await stopBackend(backendPid);
  return 0;
}

export function guardianProcessCommand(
  gatewayPid: number,
  backendPid: number,
): string[] {
  const entrypoint = Bun.main === process.execPath ? [] : [Bun.main];
  return [
    process.execPath,
    ...entrypoint,
    BACKEND_GUARDIAN_COMMAND,
    String(gatewayPid),
    String(backendPid),
  ];
}
