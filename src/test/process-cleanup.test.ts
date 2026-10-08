import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assertNoProcessesMatching, reapRecordedPids } from "./process-cleanup";

test("does not SIGKILL a pid after its start time changes", async () => {
  const scratch = mkdtempSync(join(tmpdir(), "local-base-pid-reuse-"));
  const ledger = join(scratch, "pids");
  const pid = 42_424;
  writeFileSync(ledger, `${pid}\toriginal-start\n`);
  let identityReads = 0;
  let now = 0;
  const signals: Array<[number, NodeJS.Signals | 0]> = [];

  try {
    await reapRecordedPids(ledger, {
      readIdentity: async () => {
        identityReads++;
        return {
          started: identityReads < 3 ? "original-start" : "replacement-start",
          command: "fixture",
        };
      },
      signal: (_target, signal) => {
        signals.push([pid, signal]);
        if (signal === 0)
          throw Object.assign(new Error("gone"), { code: "ESRCH" });
      },
      sleep: async (ms) => {
        now += ms;
      },
      now: () => now,
    });

    expect(signals).toContainEqual([pid, "SIGTERM"]);
    expect(signals).not.toContainEqual([pid, "SIGKILL"]);
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
});

test("reaps fixtures regardless of the inherited locale", async () => {
  const scratch = mkdtempSync(join(tmpdir(), "local-base-locale-reap-"));
  const child = Bun.spawn(
    [process.execPath, "-e", "setInterval(() => {}, 1000)", scratch],
    {
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
    },
  );
  try {
    // Bun.spawn snapshots the environment at startup, so the locale must be
    // set on a fresh process for `ps` to inherit it.
    const reaper = Bun.spawn(
      [
        process.execPath,
        "-e",
        `const { reapProcessesMatching } = await import(${JSON.stringify(
          join(import.meta.dir, "process-cleanup.ts"),
        )}); await reapProcessesMatching(${JSON.stringify(scratch)});`,
      ],
      {
        env: { ...process.env, LC_ALL: "de_DE.UTF-8" },
        stdin: "ignore",
        stdout: "ignore",
        stderr: "inherit",
      },
    );
    expect(await reaper.exited).toBe(0);
    await child.exited;
    await assertNoProcessesMatching(scratch);
  } finally {
    child.kill("SIGKILL");
    rmSync(scratch, { recursive: true, force: true });
  }
});
