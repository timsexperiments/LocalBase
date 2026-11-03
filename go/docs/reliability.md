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
make chaos          # about a minute, and more while a skip's test reruns; skips tests of open bugs
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

A skip also has to be true. A fix can reach a tree while its pull request is still open,
so `make known-bugs`, which reads the pull request's state, has nothing to say, and the
test goes on skipping behind a green run: five did, on the integration tree at `9081ff39`,
on both runners. So after a clean `make chaos` each skipped test runs again with its skip
off, and one that passes fails the run and names the line to remove. It costs nothing when
nothing is skipped. A repro that only sometimes shows its bug will trip this, and should
say so rather than stay skipped.

When the database cannot answer, someone holding something valid must not be told it is
not. With the database locked by another process, or damaged under the gateway, a good key
must get a 503 with `Retry-After`, a signed-in browser must keep its session and its cookie,
and a good sign-in link must not be told it has expired. Each of these tests first checks
the fault really stopped the lookup, so it cannot pass for the wrong reason: WAL does not
block readers on an exclusive lock, so the locked case will need another fault under WAL.

`LOCALBASE_CHAOS_RACE=1` builds the gateway with the race detector. On macOS the
full-disk tests make a small disk image; elsewhere they need a small tmpfs named by
`LOCALBASE_CHAOS_SMALL_DISK`, and skip without one.

Clock jumps are simulated by writing stored times into checkpoints and keys. The system
clock is never moved, so timers running through a real jump are not tested.
  A second run, at `91462a35` on the integration tree before its rebuild (macOS, 18
  cycles), rose from 61.1 to 78.5 MiB by cycle 7 and then only to 79.1 by cycle 18, with
  threads 12 to 15. That looks like a plateau, but it is one run. The weekly 2-hour run is
  the real answer.

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

**Waiting for the next window: does a real backend use more memory with every request?**
System-wide `available` memory (`free -m`) fell about 100 MiB per chat over four samples
and two and a half minutes on a Linux x64 runner, with the gateway running `llama-server`
on CPU (`-c 131072 --parallel 4 --cache-ram 2048`). That was not the backend's own memory,
which was not recorded, so it could include anything on the runner and page cache
accounting. Nothing yet ties it to the backend.

The gateway always passes `--cache-ram 2048`, so one hypothesis is a prompt cache filling up
to a limit. **That is unsupported until the RSS curve shows it.** The pass is
`tools/real-memory-check.sh`: 200 sequential requests with short outputs on a small model,
with the backend's own RSS as the primary measure and system memory beside it. A plateau near
+2 GiB and then flat supports the hypothesis, and a straight line past it is a leak. It runs
on a Mac, at a far smaller context than the runner used, so Mac and Linux CPU may differ: a
flat result here would not clear the Linux case.
