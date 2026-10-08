import { appendFileSync, existsSync, readFileSync } from "node:fs";

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}

/**
 * SIGTERM each pid, wait up to graceMs for exit, then SIGKILL stragglers.
 * Pids that are already gone are ignored.
 */
export async function reapPids(
  pids: Iterable<number>,
  graceMs = 3_000,
): Promise<void> {
  const targets = [...new Set(pids)].filter(
    (pid) => Number.isSafeInteger(pid) && pid > 1 && pid !== process.pid,
  );
  for (const pid of targets) {
    try {
      process.kill(pid, "SIGTERM");
    } catch {}
  }
  const deadline = Date.now() + graceMs;
  while (Date.now() < deadline && targets.some(alive)) await Bun.sleep(25);
  for (const pid of targets) {
    if (!alive(pid)) continue;
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  const killDeadline = Date.now() + 2_000;
  while (Date.now() < killDeadline && targets.some(alive)) await Bun.sleep(25);
}

/** Pids whose command line contains `needle` (never includes this process). */
export async function pidsMatching(needle: string): Promise<number[]> {
  if (!needle) return [];
  const listing = Bun.spawn(["pgrep", "-f", "--", needle], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "ignore",
  });
  const output = await new Response(listing.stdout).text();
  await listing.exited;
  return output
    .split("\n")
    .map((line) => Number(line.trim()))
    .filter(
      (pid) => Number.isSafeInteger(pid) && pid > 0 && pid !== process.pid,
    );
}

/** Kill every process whose command line mentions `needle` (a temp dir path). */
export async function reapProcessesMatching(needle: string): Promise<void> {
  await reapPids(await pidsMatching(needle));
}

/** Throws if any process command line still mentions `needle`. */
export async function assertNoProcessesMatching(needle: string): Promise<void> {
  const remaining = await pidsMatching(needle);
  if (remaining.length > 0) {
    throw new Error(
      `Leaked processes mention ${needle}: ${remaining.join(", ")}`,
    );
  }
}

export function recordPid(ledgerPath: string, pid: number): void {
  appendFileSync(ledgerPath, `${pid}\n`);
}

export function recordedPids(ledgerPath: string): number[] {
  if (!existsSync(ledgerPath)) return [];
  return readFileSync(ledgerPath, "utf8")
    .split("\n")
    .map((line) => Number(line.trim()))
    .filter((pid) => Number.isSafeInteger(pid) && pid > 0);
}
