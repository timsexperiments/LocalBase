import { appendFileSync, existsSync, readFileSync, rmSync } from "node:fs";

type ProcessIdentity = { pid: number; started: string; root?: string };

type CleanupDeps = {
  readIdentity: (
    pid: number,
  ) => Promise<{ started: string; command: string } | null>;
  signal: (pid: number, signal: NodeJS.Signals | 0) => void;
  sleep: (ms: number) => Promise<void>;
  now: () => number;
};

const defaultDeps: CleanupDeps = {
  readIdentity: async (pid) => {
    const output = await runPs(["-o", "lstart=,command=", "-p", String(pid)]);
    const line = output.trimStart().trimEnd();
    const match = /^(.{24})\s+(.*)$/.exec(line);
    return match ? { started: match[1].trim(), command: match[2] } : null;
  },
  signal: (pid, signal) => process.kill(pid, signal),
  sleep: (ms) => Bun.sleep(ms),
  now: Date.now,
};

function alive(pid: number, signal: CleanupDeps["signal"]): boolean {
  try {
    signal(pid, 0);
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
  await reapIdentities(
    targets.map((pid) => ({ pid, started: "" })),
    () => Promise.resolve(true),
    graceMs,
  );
}

async function reapIdentities(
  identities: ProcessIdentity[],
  stillSame: (identity: ProcessIdentity) => Promise<boolean>,
  graceMs: number,
  deps: CleanupDeps = defaultDeps,
): Promise<void> {
  for (const identity of identities) {
    if (!(await stillSame(identity))) continue;
    try {
      deps.signal(identity.pid, "SIGTERM");
    } catch {}
  }
  const deadline = deps.now() + graceMs;
  while (
    deps.now() < deadline &&
    identities.some(({ pid }) => alive(pid, deps.signal))
  )
    await deps.sleep(25);
  for (const identity of identities) {
    if (!(await stillSame(identity)) || !alive(identity.pid, deps.signal))
      continue;
    try {
      deps.signal(identity.pid, "SIGKILL");
    } catch {}
  }
  const killDeadline = deps.now() + 2_000;
  while (
    deps.now() < killDeadline &&
    identities.some(({ pid }) => alive(pid, deps.signal))
  )
    await deps.sleep(25);
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

async function identitiesMatching(needle: string): Promise<ProcessIdentity[]> {
  if (!needle) return [];
  const output = await runPs(["-Ao", "pid=,lstart=,command="]);
  const identities: ProcessIdentity[] = [];
  for (const line of output.split("\n")) {
    const match = /^\s*(\d+)\s+(.{24})\s+(.*)$/.exec(line);
    if (!match) continue;
    const pid = Number(match[1]);
    const started = match[2].trim();
    if (pid === process.pid || !Number.isSafeInteger(pid) || pid <= 0) continue;
    if (match[3].includes(needle))
      identities.push({ pid, started, root: needle });
  }
  return identities;
}

/** Kill every process whose command line mentions `needle` (a temp dir path). */
export async function reapProcessesMatching(needle: string): Promise<void> {
  const identities = await identitiesMatching(needle);
  await reapIdentities(
    identities,
    async (identity) => {
      const current = await defaultDeps.readIdentity(identity.pid);
      return (
        current?.started === identity.started &&
        current.command.includes(identity.root ?? "")
      );
    },
    3_000,
  );
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
export async function reapRecordedPids(
  ledgerPath: string,
  deps: CleanupDeps = defaultDeps,
): Promise<void> {
  if (!existsSync(ledgerPath)) return;
  const entries = readFileSync(ledgerPath, "utf8")
    .split("\n")
    .map((line) => /^(\d+)\t(.+)$/.exec(line))
    .filter((match): match is RegExpExecArray => match !== null);
  const targets: ProcessIdentity[] = [];
  for (const [, pidText, started] of entries) {
    const pid = Number(pidText);
    const current = await deps.readIdentity(pid);
    if (current?.started === started) targets.push({ pid, started });
  }
  await reapIdentities(
    targets,
    async (identity) =>
      (await deps.readIdentity(identity.pid))?.started === identity.started,
    3_000,
    deps,
  );
  rmSync(ledgerPath, { force: true });
}
