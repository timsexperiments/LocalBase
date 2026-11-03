# Performance

What the gateway itself costs, measured against fake backends that answer at once, so
none of these figures include inference. Real requests spend almost all their time in the
model; these numbers say how little the gateway adds to that.

```
make bench           # about 90 s; writes result.md and result.json
make bench-compare   # the same, then compares with the baseline for this platform
```

Run from `go/`. Results go to a new directory under `$TMPDIR/localbase-bench`, or
`-out <dir>` through `tools/bench.sh`. Fake backends only, ports 29000-29499.

## Baseline

macOS arm64 (64 GiB), 2026-11-02, two runs back to back on a shared machine with a load
average of 7-8. The recorded baseline is the second run,
`tools/bench/baseline-darwin-arm64.json`.

| Measure | Run 1 | Run 2 | Budget |
|---|---:|---:|---:|
| Added per request, p50 | 0.55 ms | 0.31 ms | 5 ms |
| Added per request, p99 | 2.38 ms | 0.62 ms | 25 ms |
| Gateway CPU per request | 740 µs | 410 µs | 1000 µs |
| Added to first token, p50 | 0.49 ms | 0.25 ms | 5 ms |
| Added to first token, p99 | 1.40 ms | 0.71 ms | 25 ms |
| Added per streamed token, p50 | 4.6 µs | 3.9 µs | 100 µs |
| Gateway CPU per streamed token | 13.2 µs | 11.8 µs | 100 µs |
| Memory per idle connection, RSS | 13.0 KiB | 12.6 KiB | 64 KiB |
| Goroutines per idle connection | 1 | 1 | 2 |
| Auth with 1 key, p50 / p99 | 0.13 / 0.32 ms | 0.13 / 0.23 ms | |
| Auth with 100 keys, p50 / p99 | 0.25 / 0.76 ms | 0.24 / 0.42 ms | |
| Auth with 1000 keys, p50 / p99 | 1.41 / 2.79 ms | 1.34 / 1.98 ms | 15 / 50 ms |
| Read with no writers, p50 / p99 | 0.21 / 0.55 ms | 0.20 / 1.78 ms | |
| Read with 4 writers, p50 / p99 | 1.21 / 55.2 ms | 0.87 / 55.4 ms | p99 250 ms |
| Writes per second with 4 writers | 1882 | 2026 | |
| Failed writes with 4 writers | 0 | 0 | 0 |
| Startup, new root | 70 ms | 72 ms | 1000 ms |
| Startup, existing root | 55 ms | 55 ms | 500 ms |
| First request, model load included | 260 ms | 257 ms | |
| Idle RSS | 30.9 MiB | 31.2 MiB | 128 MiB |
| Idle heap in use | 9.1 MiB | 9.1 MiB | |
| Idle goroutines / file descriptors | 10 / 13 | 10 / 13 | 40 / 40 |
| Binary size | 25.4 MiB | 25.4 MiB | |

### What the figures mean

- **Per request and first token** alternate the same request straight to the backend and
  through the gateway, so whatever else the machine is doing lands on both. The added
  figure is the difference between the two percentiles. The two runs differ by nearly a
  factor of 2 at this scale: a sub-millisecond figure on a busy machine is noisy.
- **Per streamed token** has two figures because the first understates the cost. Backend,
  gateway and client run at once, so a stage's work hides in the total time unless it is
  the slowest. The gateway's CPU per token is the real cost of relaying one; at 12 µs a
  token, a single core could carry about 80,000 tokens a second.
- **Key lookup scales linearly.** Each authenticated request reads every key
  (`Store.ResolveAPIKey`), about 1.2 µs per key per request. That is nothing at the sizes
  a local gateway sees, and would matter only at tens of thousands of keys.
- **Read with 4 writers, p99** is the cost of one writer at a time in SQLite: a read that
  meets a write waits for it. Nothing failed, and the p50 barely moved.
- **Idle connections** are kept-alive connections that made one request.
- **First request** is mostly the wait for the backend to report healthy, polled every
  250 ms, not gateway work.

The binary figure is from the `localbase_lab` build, which has a few more handlers than a
release.

## The regression guard

`make bench-compare` exits 1 when a figure is worse than the baseline for this platform by
**both** a factor of 2 **and** an absolute floor (5 ms, 50 µs, 32 MiB, 64 KiB, or 8 for
counts; throughput regresses at under half). Both are needed because a sub-millisecond
figure can double on noise alone. Baselines are per platform: a baseline from another
platform is reported as not comparable, and a platform with none only reports that.

It is meant for a nightly job, not for pull requests: the figures depend on the machine and
on what else it is doing. Budgets are recorded in `tools/bench/compare.go` and shown in the
table, but only the comparison fails a run. To record a new baseline after an intended
change, run `tools/bench.sh -write-baseline tools/bench/baseline-<os>-<arch>.json` on the
same kind of machine and commit it with the reason.

## Not measured

- **Linux x64.** There is no baseline yet. The first nightly run should record one on the
  same runner type; CI runners are slower and noisier, so its figures will differ.
- **Real backends.** Everything here uses fake backends. Inference dominates a real
  request, and a real model's load time dwarfs the first-request figure.
- **Concurrency above one request at a time** for the overhead figures. The soak covers
  behaviour under load, not its overhead.
- **Media relays** (images, speech, transcription). Only chat is measured here.
- **Sign-in and session cost.** The chaos suite measures session lookup at the store.
