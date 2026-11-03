# The system memory reserve: analysis and proposal

**No default changes here.** Changing the default is the user's decision. This document
gives the figures for it.

**The arithmetic is exact, but the proposed percent and floor are judgement, not measurements.**
No allocation probe was run on the machine behind this document, and no small Mac was tested;
see "What the evidence does not show".
 `internal/memory/reserve_table_test.go` computes every table
below through the real admission and pressure code on a fake host, and fails if a row here
differs.

## Summary

- **On an 8 GiB Mac, nothing can load today.** The 8 GiB minimum reserve equals all its memory.
- **On a 16 GiB Mac, the limit on paper is 8 GiB, and it is hard to reach.** Any model needs at
  least 10 GiB free, a 4 GiB model needs 12, and an 8 GiB model needs all 16.
- **On 64 GiB, the reserve is 9.6 GiB**, so a 24 GiB model needs 33.6 GiB free. That is why it
  was refused with about 31 GiB free.
- **Recommendation: Option C, 10% with a 3 GiB minimum.** It needs no new setting, and it can be
  tried now in config. It should be checked on a real 8 or 16 GiB Mac first; see "What the
  evidence does not show".

## How the reserve works

`reserve = max(minimum, percent of total)`. The default for a new root is 10% and 3 GiB; it was 15% and 8 GiB. From it:

| Check | Rule | Result |
| --- | --- | --- |
| Can never fit | model > total - reserve, or model > GPU working-set limit | 422 `model_too_large` |
| Pressure, no new loads | free < 1.25 x reserve | 503 `insufficient_memory`, `Retry-After` |
| Not enough free now | free - pending - model < reserve | 503 `insufficient_memory`, `Retry-After` |
| Pressure monitor, critical | free < reserve | Running models are **stopped** |

So free needed for a model is `max(1.25 x reserve, model + reserve)`. "Free" is free +
speculative + inactive pages. The GPU working-set limit is 2/3 of memory up to 36 GiB and 3/4
above, or `LOCALBASE_GPU_WORKING_SET_GB`. Models are shown as if their whole demand is
accelerator memory. A real model's demand is its estimate, which can exceed its file size.

**Lowering the reserve moves all of these lines together.** On 64 GiB, Option C lowers the point
where running models are stopped from 9.6 to 6.4 GiB free. That is the main risk in any change.

## Today's figures

### Today: 15%, at least 8 GiB

| Total GiB | Reserve (stops models below this) | No new loads below | Free needed, 1 GiB model | Free needed, 4 GiB | Free needed, 8 GiB | Largest model ever |
| --- | --- | --- | --- | --- | --- | --- |
| 8 | 8.0 | 10.0 | can never load | can never load | can never load | 0.0 |
| 16 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 8.0 |
| 24 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 16.0 |
| 32 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 21.3 |
| 64 | 9.6 | 12.0 | 12.0 | 13.6 | 17.6 | 48.0 |
| 128 | 19.2 | 24.0 | 24.0 | 24.0 | 27.2 | 96.0 |

What that means for a Mac:

- **8 GiB:** nothing can ever load. With #411 the answer is 422; without it, a 503 whose retry
  can never work. The monitor also reads critical whenever free memory is under 8 GiB, which on
  this machine is always.
- **16 GiB:** models up to 8 GiB pass the "can never fit" check. An 8 GiB model then needs 16 GiB
  free, which cannot happen on a running system, and it gets a 503 rather than a 422 (see
  "Refusals"). Anything loaded is stopped as soon as free memory drops under 8 GiB.
- **24 and 32 GiB:** the same 10 GiB and 12 GiB free requirements, with more room to meet them.

## TS parity

The same formula and defaults: `effectiveMemoryReserveBytes` and `defaultMemorySafetyConfig` in
`src/domains/runtime/memory-safety.ts`, with the same 1.25 multiplier and the same critical and
constrained lines. On a Mac, both providers report the pressure as `normal` and leave the lines
above to decide. A new default would be a deliberate difference from TS.

One existing difference: TS asks Metal for the GPU working-set limit and falls back to the
heuristic, and Go only has the heuristic until #427 lands. This 64 GiB Mac really reports 53084 MiB
(0.81 of memory, 51.8 GiB) against the heuristic's 48 GiB. So the last column of every table, which
uses the heuristic, is lower than a real Mac may allow once that lands, and this is separate from the
reserve. Other Mac sizes are unmeasured.

## Options

The form is `clamp(percent of total, floor, cap)`.

### Option A: 10%, at least 2 GiB, at most 8 GiB

| Total GiB | Reserve (stops models below this) | No new loads below | Free needed, 1 GiB model | Free needed, 4 GiB | Free needed, 8 GiB | Largest model ever |
| --- | --- | --- | --- | --- | --- | --- |
| 8 | 2.0 | 2.5 | 3.0 | 6.0 | can never load | 5.3 |
| 16 | 2.0 | 2.5 | 3.0 | 6.0 | 10.0 | 10.7 |
| 24 | 2.4 | 3.0 | 3.4 | 6.4 | 10.4 | 16.0 |
| 32 | 3.2 | 4.0 | 4.2 | 7.2 | 11.2 | 21.3 |
| 64 | 6.4 | 8.0 | 8.0 | 10.4 | 14.4 | 48.0 |
| 128 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 96.0 |

### Option B: 10%, at least 3 GiB, at most 8 GiB

| Total GiB | Reserve (stops models below this) | No new loads below | Free needed, 1 GiB model | Free needed, 4 GiB | Free needed, 8 GiB | Largest model ever |
| --- | --- | --- | --- | --- | --- | --- |
| 8 | 3.0 | 3.8 | 4.0 | 7.0 | can never load | 5.0 |
| 16 | 3.0 | 3.8 | 4.0 | 7.0 | 11.0 | 10.7 |
| 24 | 3.0 | 3.8 | 4.0 | 7.0 | 11.0 | 16.0 |
| 32 | 3.2 | 4.0 | 4.2 | 7.2 | 11.2 | 21.3 |
| 64 | 6.4 | 8.0 | 8.0 | 10.4 | 14.4 | 48.0 |
| 128 | 8.0 | 10.0 | 10.0 | 12.0 | 16.0 | 96.0 |

### Option C: 10%, at least 3 GiB, no cap

| Total GiB | Reserve (stops models below this) | No new loads below | Free needed, 1 GiB model | Free needed, 4 GiB | Free needed, 8 GiB | Largest model ever |
| --- | --- | --- | --- | --- | --- | --- |
| 8 | 3.0 | 3.8 | 4.0 | 7.0 | can never load | 5.0 |
| 16 | 3.0 | 3.8 | 4.0 | 7.0 | 11.0 | 10.7 |
| 24 | 3.0 | 3.8 | 4.0 | 7.0 | 11.0 | 16.0 |
| 32 | 3.2 | 4.0 | 4.2 | 7.2 | 11.2 | 21.3 |
| 64 | 6.4 | 8.0 | 8.0 | 10.4 | 14.4 | 48.0 |
| 128 | 12.8 | 16.0 | 16.0 | 16.8 | 20.8 | 96.0 |

**Why Option C.** Options B and C differ only above 80 GiB. A cap needs a new stored setting; the
floor and percent exist already. Option C on 128 GiB keeps a larger margin, and if anyone wants
less they can set it. Option A's lower floor gets a 1 GiB model running at 3 GiB free on small
Macs, but it stops running models only below 2 GiB, and nothing here shows that is safe.

**The 24 GiB model on 64 GiB:** Option C needs 30.4 GiB free, so 31 GiB would admit it
(`TestReserveUserCase`). The margin is thin, and this Mac read about 29 GiB in the sample below.

**A new default only reaches new roots.** The reserve is stored in each root's config, so existing
installs keep theirs until someone changes it.

## What the evidence does not show

I sampled this 64 GiB Mac for one minute, read-only, with the live gateway running and a model
loaded:

| Reading | Value |
| --- | --- |
| Free as LocalBase counts it | 27.2 to 29.0 GiB |
| Wired | 3.3 to 4.6 GiB |
| Compressor | 4.1 GiB |
| `kern.memorystatus_vm_pressure_level` | 1 (normal), throughout |
| `kern.memorystatus_level` | 86 to 88% |

This shows only that macOS reports no pressure at about 29 GiB free. It does not show where
pressure begins. **The floor and percent above are judgement, not measurements.**

I did not run an allocation probe. Finding the point where pressure starts means pushing free
memory far below the live gateway's own 12 GiB and 9.6 GiB lines, which would refuse loads and stop
its models. A useful probe needs either a window with the live gateway stopped or, better, a real
small Mac.

### The check to run on a real 8 or 16 GiB Mac

Do this before a default changes. `tools/reserve-check.sh` has not been run yet: it passed syntax
checks only, because the machine here is the wrong size and runs the live gateway.

```sh
tools/reserve-check.sh sample 300     # read-only, during normal use with a browser and apps open
tools/reserve-check.sh probe          # allocates, in steps; save your work first
```

`probe` refuses to start if LocalBase or a model runtime is running, or on more than 24 GiB. It
stops at 4 GiB free (above Option C's 3.75 GiB line), at any macOS pressure above normal, at half
the machine's memory, or after 5 minutes. It holds random data, so the compressor can't hide it.

Things to record:

1. How much is free in normal use. That decides whether the "free needed" column can be met at all.
2. At what free figure the pressure level or compressor moves, if it does before the probe stops.
3. Whether the machine stays responsive at the point it stopped.

To test closer to a line, pass a lower floor (`probe 3`), at your own judgement.

The kernel's own pressure level could also feed the monitor, which today cannot see it on a Mac.
`kern.memorystatus_level` reads far higher than LocalBase's free figure here (about 87% against
45%), so the two measure different things. Neither is changed here.

## Refusals

A reserve that makes a model impossible is already distinct, through #411: 422 `model_too_large`,
no `Retry-After`, with a message saying it can't load even when nothing else is running. Memory
that is low right now stays 503 `insufficient_memory` with `Retry-After`.
`TestReserveRefusalsAreDistinct` pins both on a gateway whose reserve alone decides.

### Known gap: fits on paper, can never run on a live system

An 8 GiB model on 16 GiB passes the "can never fit" check, because it fits an empty machine, but it
needs all 16 GiB free, so it can never load. It answers 503 with `Retry-After`, and the retry will
not help. A 422 would be the honest answer.

**The missing input is a figure for what the system always holds** (wired memory, the kernel and
core services). With it, the check could become `model > total - reserve - always held`. LocalBase
has no such figure today. It would also differ from TS. Option C narrows the cases but doesn't remove
them. Left open on purpose.

## Changing the reserve

In a TOML file, then `local-base configure --config <file>`, followed by a restart:

```toml
[memory.systemReserve]
percent = 10
minimumGb = 3
```

This is Option C, the default for a new root. A root already initialized keeps the values stored in its database until changed this way. There is no cap setting and no environment variable for the
reserve. `LOCALBASE_GPU_WORKING_SET_GB` overrides only the GPU limit.
