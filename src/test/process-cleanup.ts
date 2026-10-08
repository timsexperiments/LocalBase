import { appendFileSync, existsSync, readFileSync, rmSync } from "node:fs";

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

async function runPs(args: string[]): Promise<string> {
  const proc = Bun.spawn(["ps", ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const [output, stderr, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  if (code !== 0 && code !== 1) {
    throw new Error(`ps ${args.join(" ")} exited ${code}: ${stderr.trim()}`);
  }
  return output;
}

/** Pids whose command line literally contains `needle` (never this process). */
export async function pidsMatching(needle: string): Promise<number[]> {
  if (!needle) return [];
  const output = await runPs(["-Ao", "pid=,command="]);
  const pids: number[] = [];
  for (const line of output.split("\n")) {
    const match = /^\s*(\d+)\s+(.*)$/.exec(line);
    if (!match) continue;
    const pid = Number(match[1]);
    if (pid === process.pid || !Number.isSafeInteger(pid) || pid <= 0) continue;
    if (match[2].includes(needle)) pids.push(pid);
  }
  return pids;
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

/** Process start time (stable identity for a pid), or null if it is gone. */
async function startTime(pid: number): Promise<string | null> {
  const output = (await runPs(["-o", "lstart=", "-p", String(pid)])).trim();
  return output || null;
}

/** Record `pid` with its start time so a reused pid is never signalled. */
export async function recordPid(
  ledgerPath: string,
  pid: number,
): Promise<void> {
  const started = await startTime(pid);
  if (started) appendFileSync(ledgerPath, `${pid}\t${started}\n`);
}

/**
 * SIGTERM/SIGKILL recorded pids whose start time still matches, then drop the
 * ledger. Entries for exited or reused pids are pruned without signalling.
 */
export async function reapRecordedPids(ledgerPath: string): Promise<void> {
  if (!existsSync(ledgerPath)) return;
  const entries = readFileSync(ledgerPath, "utf8")
    .split("\n")
    .map((line) => /^(\d+)\t(.+)$/.exec(line))
    .filter((match): match is RegExpExecArray => match !== null);
  const targets: number[] = [];
  for (const [, pidText, started] of entries) {
    const pid = Number(pidText);
    if ((await startTime(pid)) === started) targets.push(pid);
  }
  await reapPids(targets);
  rmSync(ledgerPath, { force: true });
}
