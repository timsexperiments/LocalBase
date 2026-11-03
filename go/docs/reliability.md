# Reliability tools

Black-box tests of a running gateway under load and failure. They run a real
binary with real child processes, drive it only through HTTP and signals, and
use fake backends and a fake memory host, so they need no models, no GPU and no
network.

## The soak

```
make soak-smoke     # one cycle, about 2 minutes
make soak           # 30 minutes
make soak-race      # one cycle against a race-detector gateway
```

Run these from `go/`. Each writes `result.md` (one page) and `result.json` to a
new directory under `$TMPDIR/localbase-soak`, or `-out <dir>` through
`tools/soak.sh`, and exits non-zero on any violation. A failing run keeps its
scratch root and logs and prints where they are.

### What a cycle does

| phase | fake host | what it adds | refusals it allows |
|---|---|---|---|
| warm (first cycle only) | 48 GiB free | clients start; first resource sample | none |
| steady | 48 GiB | every client class | none beyond 429 |
| burst | 48 GiB | FIFO order, queue full, queue deadline, run limit, image queue | none beyond 429 |
| unload | 48 GiB | unloads of every model | `model_unloading` |
| tight | 17 GiB | the small models fit, the large ones do not | `insufficient_memory` |
| blind | unmeasurable | new loads are refused, running models serve | `insufficient_memory` |
| constrained | 11 GiB, or a pressure reading | idle runtimes stop, new loads are refused | `insufficient_memory` |
| critical | 8 GiB, or a pressure reading | every runtime stops | `insufficient_memory`, `service_unavailable`, cut streams |
| recovery | 48 GiB | service returns after three normal samples | as critical, while it recovers |
| checkpoint | 48 GiB | clients pause, every model unloads, resources are sampled | |

The last stage of every run is a SIGTERM under load followed by a restart on the
same root.

The clients are chat with and without streaming, a chat model that forces
switches, embeddings, decisions, images, speech, transcription, detached runs
(dropping and resuming their event streams, stopping some and leaving some
behind), image runs, health and model lists, and unloads.

### What fails a run

- Any 5xx outside its phase's allow-list, or one without an error envelope and
  `Retry-After`. Any 500 or 502.
- A 429 without its documented code and `Retry-After`.
- A success that is wrong: an answer from another model, a stream without
  `[DONE]`, a run missing output, an image that is not a PNG.
- A connection error before the drain.
- Queues: out-of-order completion, a full queue that does not refuse at once, a
  queue deadline missed by more than 1.5 s, a run limit that does not hold.
- Admission: a runtime started when the fake host did not have its demand above
  the reserve, or while memory was not healthy; a refusal whose figures do not
  add up. Read from the gateway's own events, against the host's history.
- Pressure: transitions other than healthy, constrained, critical, healthy each
  cycle, or late. Critical memory that leaves a backend running.
- Resources: growth between the first and last checkpoints beyond the budgets in
  `tools/soak/checks.go`, or a climb across the second half of a long run.
- Drain: a non-zero exit, over 25 s, short in-flight streams not finishing, a
  backend left running, a checkpoint still claiming to run, a restart that does
  not serve.
- A panic, an unexpected error event, a data race under `-race`, or an API key or
  prompt in the log.

### Reading the table

Latency figures are successes only. A detached run's figure is its whole life.
`worker spread` is the least over the most successes among one class's workers,
so 1.00 is perfectly even.

Runs of four or more checkpoints also report each resource's change per cycle, a
least-squares fit that leaves out warm-up. The budgets only catch a leak once it
is large; the slope shows a small one as a number. It does not fail a run, because
over a short run a slope can be a plateau still settling.

### Open questions

- **RSS and threads over a long run.** In the first 30-minute soak (macOS, 18
  cycles, 2026-11-02), goroutines and descriptors never moved and heap was flat
  from cycle 3, while RSS rose about 3.5 MiB from cycle 2 to cycle 18 (64.3 to
  67.8 MiB) and threads went 14 to 16. That is inside the budgets, but 28 minutes
  cannot tell a plateau from a very slow leak. The weekly long run should answer
  it; until then this is open, not a pass.

## Chaos

```
make chaos          # about 40 s; skips tests of open bugs
make chaos-all      # includes them
make known-bugs     # fails if a skip names a merged pull request, or none
```

`tools/chaos` breaks a real gateway on purpose, one test and one gateway at a time:
backends killed mid-stream and mid-request, crash loops, the gateway itself killed, a
locked and a damaged database, runs across a kill among damaged checkpoints, stored
times from another clock, dropped and corrupt downloads, a stalled peer, a full disk,
huge, slow and malformed requests, and a thousand keys and sessions.

A test of an open bug starts with `knownBug(t, <pr>, "...")`, naming the pull request
that fixes it. That fix removes the line.

`LOCALBASE_CHAOS_RACE=1` builds the gateway with the race detector. On macOS the
full-disk tests make a small disk image; elsewhere they need a small tmpfs named by
`LOCALBASE_CHAOS_SMALL_DISK`, and skip without one.

Clock jumps are simulated by writing stored times into checkpoints and keys. The system
clock is never moved, so timers running through a real jump are not tested.
  A second run, at `91462a35` on the integration tree before its rebuild (macOS, 18
  cycles), rose from 61.1 to 78.5 MiB by cycle 7 and then only to 79.1 by cycle 18, with
  threads 12 to 15. That looks like a plateau, but it is one run. The weekly 2-hour run is
  the real answer.

  The result files of both runs were lost in a reboot on 2026-11-03, so they stand as reported and cannot be
  re-examined. The second run's numbers: PASS, 18 cycles, 28m8s, 44,321 operations, 0 violations, 28 of 28
  checks, 1,111 model starts, 18,781 refusals checked, 54 pressure transitions, drain in 10.013 s with 3 of 3
  short streams finished, restart healthy in 60 ms.

  | Checkpoint | Goroutines | Descriptors | Threads | Heap, MiB | RSS, MiB |
  |---|---:|---:|---:|---:|---:|
  | After warm-up | 10 | 14 | 12 | 11.0 | 61.1 |
  | After cycle 3 | 10 | 14 | 14 | 12.1 | 68.5 |
  | After cycle 7 | 10 | 14 | 14 | 12.4 | 78.5 |
  | After cycle 10 | 10 | 14 | 14 | 12.4 | 78.7 |
  | After cycle 14 | 10 | 14 | 14 | 12.5 | 78.9 |
  | After cycle 18 | 10 | 14 | 15 | 12.6 | 79.1 |

## The lab

`tools/lab` is what the tools share: it builds the gateway with the
`localbase_lab` tag and the fake backend, creates a scratch root, and starts and
samples gateways.

The tag adds two things, neither of them in a release:

- `internal/memory/host_lab.go`: with `LOCALBASE_LAB_HOST_FILE` set, host memory
  comes from a file the harness rewrites.
- `cmd/localbase/lab_debug.go`: with `LOCALBASE_LAB_DEBUG_ADDR` set, goroutine,
  descriptor and heap counts and pprof on that address.

`TestDefaultBuildHasNoLabSeam` in `cmd/localbase` builds the default binary and
fails if either is in it.

The fixture removes artifact hashes through a `catalog.d` patch. Without a hash a
gateway records no observed peak, so a fake backend's small footprint never
replaces a model's real demand and admission decides the same on macOS and Linux.

### Rules for a shared machine

- Ports come from 29000-29499; `LOCALBASE_LAB_PORTS=<low>-<high>` moves the block.
- Built tools go to `~/.cache/localbase/`, or `LOCALBASE_TOOLS_CACHE`; see `tools/README.md`.
- A scratch directory holds everything. `HOME` points into it, proxies point
  nowhere, and the caller's `LOCALBASE_*` and token settings are removed.
- The tools refuse a scratch path inside the installed root.
- `tools/soak.sh` gives a run its own session, lower priority, a stop on any
  signal and a watchdog. A watcher process stops the gateway if the harness dies.

## Linux

The soak runs on Linux through `make soak-smoke`; CI runs it nightly on ubuntu.
Two things differ from macOS, and both are expected:

- **Backend selection.** A machine with no GPU has no automatic choice for some
  models, and linux/arm64 has no build of the built-in media backends at all. The
  fixture then selects its own `lab-*` backend definitions through the API. They
  stay out of automatic selection, so a machine with real choices keeps them.
- **Queue timeouts in tight memory.** Linux has no footprint sampler, so a refused
  model credits idle runtimes at their full reservation, stops them and waits up
  to 10 s for the memory to appear, holding queues meanwhile. The fake host never
  frees memory, so that wait always runs out and `inference_queue_timeout` shows
  up. A real host would usually settle sooner; how much sooner has not been measured.

## Real-model checks

The chaos tests use fake backends, so these questions were checked against the real
runtime on 2026-11-02, in a window from the operator: `llama-server` 0.7.0-dev
(build 11671, commit `ba3c05f71`), `Qwen3-Embedding-0.6B`, from APFS clones, context
2048, one slot, a 1.64 GiB plan.

| # | Question | Answer | Evidence |
|---|---|---|---|
| 1 | Does `llama-server` exit when its port is taken, and does it load the model first? | It exits without loading. | Exit 1 in 0.09 s (3 s on a cold first run), `couldn't bind HTTP server socket`, no load lines, 217 MB peak footprint. |
| 2 | Does an orphan answer `/health`? | Yes. | After the gateway's SIGKILL: parent pid 1, `/health` 200, `/props` names its model. |
| 3 | Does an orphan survive its next log line, now that its pipes have no reader? | Yes. | 6 requests and 60 s idle, all answered. A process with its pipes' readers killed: 3 requests answered, still alive. Whether it ignores SIGPIPE or wrote nothing was not determined. |
| 4 | What does an orphan cost in memory? | It stays resident, and the gateway has no record of it. | RSS 993 MB, unchanged at +60 s; `phys_footprint` 474 then 295 MB. The host's available figure was too noisy to measure it closely. |
| 5 | What does a successor see? | What the fake showed. | `runtime.ready` 1 ms after `runtime.starting`, from the orphan's health; the new process exits with status 1 within about 60 ms; the request gets a 200 from the orphan; unload answers 404 `model_not_loaded`. |

The fake backend's bind failure matches, so `TestGatewayKilledOutright` is a fair test
of this path.

**Not checked on a real runtime:** a request for a different model answered by an
orphan (the window covered one model), the media runtimes (`whisper-server`,
`sd-server`, `tts-server`), and memory at a large model's scale.

**To run more of these:** only in a window from the operator, who first checks that
the live gateway is idle. Work from clones in a scratch directory, never from the live
root. Check live `/health` and free memory (free + speculative + inactive pages) just
before starting and throughout, and stop at once if the live state changes or free
memory falls under the agreed floor. Print the plan the gateway will load before
loading it, and stop if it is over the agreed size: a model's default plan can be far
larger than its file (this model's was 5.3 GiB with 4 slots and a 32768 context).

**Does a real backend use more memory with every request?** On a Mac, up to its prompt cache's
limit, and then no. Measured on 2026-11-02 with `tools/real-memory-check.sh`: `llama-server` 0.7.0-dev
(build 11671, `ba3c05f71`), Metal, `qwen2.5-coder-1.5b-instruct-q4_k_m`, context 8192, one slot, 200
sequential requests of about 2,400 tokens, each a new prompt, with 8 tokens out. All returned 200.

| Request | 1 | 25 | 50 | 75 | 100 | 125 | 150 | 175 | 200 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Backend RSS, MiB | 1,346 | 2,180 | 3,052 | 3,644 | 3,749 | 3,609 | 3,609 | 3,437 | 3,647 |

RSS rose 34.80 MiB a request over the first 50, and moved -0.55 MiB a request over the last 100. The
gateway always passes `--cache-ram 2048`, and the backend's log shows that cache filling: 141 lines
of `making room for prompt cache entry, removing oldest entry`, averaging 34.74 MiB, and 200 - 141 =
59 entries, which is what 2,048 MiB holds at that size. The planner already reserves this 2 GiB. The
plan was 4,079 MiB, and RSS peaked at 3,749 MiB.

RSS went on rising about 350 MiB after evictions began, for 2,403 MiB in all, before it stopped. Allocator
overhead would fit, but that was not measured.

**What this does not settle:**

- **What the Linux runner showed.** System-wide `available` (`free -m`) fell about 100 MiB per chat over
  four samples, with the gateway running `llama-server` on CPU (`-c 131072 --parallel 4 --cache-ram 2048`).
  That was not the backend's own memory, which was not recorded, so it could include anything on the runner
  and page cache accounting. **It remains unexplained.** If it is the same cache, RSS would flatten at
  about +2 GiB after about 20 requests. That is a prediction, to be checked on a real Linux run by
  recording the backend's RSS and keeping its log.
- **Other models and shapes.** An entry's size depends on the model, the prompt and the context, so
  growth per request will differ.
- **A long run.** 200 requests is about 140 evictions.
- **Whether an orphan ignores SIGPIPE.** macOS `ps` cannot show ignored signals. On Linux,
  `SigIgn` in `/proc/<pid>/status` would answer it.

**The raw evidence is gone.** A reboot on 2026-11-03 emptied `/private/tmp`, which held this run's CSV and
log, so these results stand as reported and cannot be re-examined. The numbers kept, from `#521` at
`48e66cf8`, with the window running 22:59:19 to 23:04:10 on 2026-11-02:

| Measure | Values |
|---|---|
| Plan | 4,278,105,664 bytes, `derived`, also the real start's |
| Backend RSS, MiB, at request | 1: 1,346, 10: 1,659, 20: 2,006, 25: 2,180, 40: 2,703, 50: 3,052, 58: 3,330, 59: 3,364, 60: 3,399, 65: 3,469, 70: 3,609, 75: 3,644, 80: 3,714, 90: 3,714, 100: 3,749, 125: 3,609, 150: 3,609, 175: 3,437, 200: 3,647 |
| Backend RSS, requests 100-200 | mean 3,583, least 3,433, most 3,749 MiB |
| `phys_footprint`, MB, at request | 1: 380, 11: 727, 21: 1,075, 31: 1,423, 41: 1,772, 51: 2,121, 61: 2,469, 71: 2,678, 81: 2,748, 91: 2,748, 101: 2,783, 111: 2,502, 121: 2,643, 131: 2,643, 141: 2,643, 151: 2,678, 161: 2,713, 171: 2,467, 181: 2,467, 191: 2,572 |
| Evicted cache entries | 141: mean 34.74, least 34.223, most 35.458 MiB |
| Requests | 200 of 200 returned 200; 2,345-2,432 prompt tokens, mean 2,385; mean 1,415 ms, longest 5,424 ms (the first, with the load) |

System-wide memory (free + speculative + inactive), not the backend's, in MiB: request 1: 30,551, 25: 30,060,
50: 29,495, 75: 29,179, 100: 29,210, 125: 29,425, 150: 29,348, 175: 29,428, 200: 29,505; least 28,899, at
request 86. Its early part overlaps another thread's build and tests.
