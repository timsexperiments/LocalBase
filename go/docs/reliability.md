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
- A scratch directory holds everything. `HOME` points into it, proxies point
  nowhere, and the caller's `LOCALBASE_*` and token settings are removed.
- The tools refuse a scratch path inside the installed root.
- `tools/soak.sh` gives a run its own session, lower priority, a stop on any
  signal and a watchdog. A watcher process stops the gateway if the harness dies.
