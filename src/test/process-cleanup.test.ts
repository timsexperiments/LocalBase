import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { reapRecordedPids } from "./process-cleanup";

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
