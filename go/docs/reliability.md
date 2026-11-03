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

Available memory fell about 100 MiB per request over four samples on a CPU `llama-server`
on a Linux x64 runner (`-c 131072 --parallel 4`). Four samples cannot tell a cache filling
from a leak. The plan is about 200 sequential requests to a small model on a scratch
gateway on a Mac, with the same conditions as above, recording the backend's RSS and
available memory throughout: flat after warm-up, or falling in a line? Use a mix of new and
repeated prompts, and check whether the build has a host-memory prompt cache (`--cache-ram`
in recent `llama-server` builds): if it does, a run with the cache off separates the two.
The plan must fit the agreed size, so the context will be far smaller than the one observed
and the platform differs. A flat result here would not clear the Linux case; say so if
that is the result.

## The browser suite

```
make e2e                              # about a minute
E2E_PART=journeys tools/e2e.sh        # the journeys alone, under a minute
E2E_PAGES=chat,studio E2E_WIDTHS=390 E2E_THEMES=dark tools/e2e.sh
```

Run from `go/`. It needs `bun`. Chrome comes from `LBGO_CHROME`, or else the Chrome for Testing pinned
in `tools/e2e/pins` is fetched into a cache (`LOCALBASE_E2E_CACHE`, default `~/.cache/localbase-e2e`),
checked against its sha256 and never installed, so that every run checks the same browser. Only a
platform with no pin, such as linux/arm64, falls back to an installed Chrome. axe-core comes the
same way from `tools/axe.pins`. Headless, fake backends, a fake memory host, and
ports from the lab's block. Results and captures go to a new directory under
`$TMPDIR/localbase-e2e`, or `-out <dir>`.

Exit 0 means everything passed, exit 1 means findings (in `result.md`, with captures), and exit 2 means
the suite itself could not run.

**On Ubuntu,** `kernel.apparmor_restrict_unprivileged_userns=1` stops Chrome's sandbox, and Chrome never
answers. On a disposable runner, set it to 0 and the suite runs with the sandbox on, as CI does
(`ubuntu-latest`). `LOCALBASE_E2E_NO_SANDBOX=1` adds `--no-sandbox` for a machine where that setting can't
be changed; CI doesn't need it. When Chrome fails to start, the error prints the end of its log and the
scratch root is kept.

**Journeys** sign in with a first-run link from `local-base access login-link`, check it works once,
hold a first chat, stop a stream, keep HTML in a reply inert, use Image, Speech, Transcribe and
Decisions, and then break things: too little memory, a backend killed mid-reply, every session
ended. The driver's small control endpoint does what a page cannot, such as changing the host's memory.

After memory comes back, chat goes on refusing for about 4.6 s while the strip already shows it free. That
is intended: recovery takes three normal samples at a 2 s poll (`recoverySamples` in
`internal/memory/controller.go`), so that one good reading does not undo the hysteresis. The journey
allows 30 s, and prints the time it measured.

**The matrix** runs every page at 320, 360, 390, 430, 768 and 1280 px in light and dark:

- **Overflow.** `scrollWidth` is compared with the width asked for, less a classic scrollbar where
  there is one (15 px on Linux), and `clientWidth` must not exceed the width asked for. `innerWidth` is
  not used: in mobile emulation it can grow with the content, so `scrollWidth - innerWidth` can read 0 on
  a page that overflows. Self-tests plant a 1200 px element, a page that fits beside a scrollbar and one
  5 px too wide beside it.
- **axe-core**, WCAG 2.0, 2.1 and 2.2 A and AA, and any exception or 5xx on the page.

Per page, at 390 and 1280 px:

- **Keyboard and focus.** Tab reaches every visible control with no trap. A radio group is one stop, a
  closed `<details>` holds none, and a tabindex of -1 is out of the order on purpose. Focus is on screen,
  the middle of the control is not covered, and focusing it changes how it looks: the control is read
  focused and then blurred, along with its pseudo-elements, up to three boxes around it (a composer
  shows focus on its frame) and the element after it (a hidden checkbox styles its neighbour).
- **Sheets.** Enter opens one, focus moves in and stays in, Escape closes it, and focus returns to what
  opened it.
- **Reduced motion.** Nothing keeps animating when reduced motion is asked for.

**Every check is shown a page that breaks it.** Before the matrix, the suite plants an overflow, an
exception, an image with no alt text, a control with no focus style, one Tab cannot reach, a trap, a
control off screen, one under a cover, an opener that does nothing, a sheet that lets focus out and
ignores Escape, one that returns focus to the wrong place, and a spinning element under reduced motion.
If a check misses its plant the suite stops with exit 2, since a clean matrix would mean nothing.
`result.md` lists what was proved. A new check needs a plant of its own.

**Waiting.** Everything waits on the page's state through `until`, which polls and, if the state never
comes, fails with a screenshot, the address and the console. There are no fixed sleeps and no retries, so a
flaky check fails loudly.

**Adding to it.** A new page goes in `ALL_PAGES` in `tools/e2e/run.ts`. A new journey is an
`await journey("what a person sees", async () => { ... })`: use `until` to wait, and `need` to assert.

Not covered: video generation, installing a first model from an empty root (no chat model means the
welcome page's install flow), voice recording, an identity provider's sign-in, Safari and Firefox, and
real devices. The focus check shows that something changes, not that the change has enough contrast;
the buttons inside a native audio or video player are taken on trust; the keyboard, sheet and motion
checks run in the light theme on the page as it first loads, so a sheet's own contents, a result on the
page and a state reached by using it get axe and overflow only where a journey or a sheet reaches them.
