# check-manifest

A check inside a guard that quietly stays off prints nothing, so the run still says all pass. Counting doesn't help, because
nobody notices a total that's a few short. This compares what should have run with what did, and exits 1 on a difference.

    bun tools/check-manifest/run.ts check tools/chat-runs-check/run.ts <log> [--mode=<name>] [--allow=<file>]
    bun tools/check-manifest/run.ts phone tools/phone-check/run.ts <log>
    bun tools/check-manifest/run.ts axe tools/axe-check/run.ts <log>

`check` reads every `check(...)` call in the source and requires a result line for it. Where a run legitimately takes one of two
branches, an allow file names what may be absent and why: mode, words from the message, reason, separated by tabs.

**Prove it for your suite:** guard a section off with `if (false)` for one run. The manifest has to list that section's checks.

**What it can't see:** it knows a check ran at least once, not that it ran at every width in its loop. A message with no words of
its own is reported as unverifiable. Another suite's helper with a different name needs its own pattern.
