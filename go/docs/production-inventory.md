# Production inventory

Every productionized behaviour of the Go gateway and CLI, what guards it, and what
doesn't. Written before any cleanup (2026-11-02, on the tree of #403): a refactor of an
area must not start until that area's gaps below are closed with characterization tests.

**Status:** **Guarded** a test fails if it breaks. **Partial** the main path is
guarded, but named branches aren't. **GAP** nothing automated fails if it breaks.

**Where a test runs matters as much as whether it exists:**

| Mark | Runs | Meaning |
|---|---|---|
| (none) | every PR, in `go.yml` | a unit test, or a contract test under `-impl=go` |
| **D** | only by hand: `-impl=both` | needs the TS gateway |
| **R** | only by hand: real models | needs `LOCALBASE_CONTRACT_RUNTIMES` and weights |

## 1. The biggest gap: CI never compares Go with TS

`go.yml` runs `tools/contract.sh -args -impl=go`. Every differential and most CLI tests
need TS and are **skipped in CI**. `ci.yml`'s `bun test` runs only on PRs into main and pushes to main. So
"the dual suite stays green" is currently only true when someone runs it by hand, and
nothing in CI would stop a PR breaking TS parity, including this cleanup.

**Proposal (for the OSS thread's workflows; not built here):** a `go-dual` job on
`macos-14`.
- **When:** nightly on a schedule, plus `workflow_dispatch`, plus any PR labelled `dual`.
  Not on every PR: it takes minutes and needs a model file.
- **What:** `bun install --frozen-lockfile`, then `tools/contract.sh -args -impl=both`.
  Keep the failing run's log as an artifact, as `go.yml` does.
- **The model:** the TS gateway won't start without its active model's files, so the job
  downloads the pinned `qwen2.5-coder-1.5b-instruct-q4_k_m.gguf` (about 1 GB, public) into
  `actions/cache` and points `LOCALBASE_CONTRACT_MODELS` at it. No inference runs, so it
  fits a runner's memory.
- **Until TS retires,** the rule for this stack: run `-impl=both` locally before a PR goes
  ready, and say so in the PR. Remove the job when TS retires.

Real-model tests (**R**) stay manual: no runner has the weights or memory.

## 2. Baseline

Statement coverage, darwin/arm64, from `tools/coverage.sh both`: unit tests plus the dual
contract suite with no real models. Neither alone is the true figure: per-package
`go test -cover` said 50.7%, because the contract suite drives the Go binary from outside.
`internal/web` is here for completeness; the FE thread owns it.

Real-model runs aren't counted, so these slightly understate. Linux-only code counts as
uncovered, since this ran on macOS.

| Package | Coverage | Statements | Source lines | Test lines |
|---|---|---|---|---|
| cmd/localbase | 63.1% | 3264 | 6018 | 337 |
| internal/acquire | 66.6% | 629 | 1307 | 396 |
| internal/auth | 76.8% | 1371 | 2810 | 563 |
| internal/backends | 84.6% | 376 | 985 | 373 |
| internal/catalog | 85.3% | 1085 | 2355 | 462 |
| internal/cli | 84.4% | 231 | 492 | 74 |
| internal/collate | 74.2% | 31 | 71 | 31 |
| internal/conf | 94.7% | 452 | 909 | 229 |
| internal/gateway | 73.6% | 5830 | 10713 | 3379 |
| internal/hub | 82.5% | 1081 | 2228 | 364 |
| internal/install | **37.2%** | 266 | 513 | 0 |
| internal/jsjson | 94.2% | 155 | 304 | 37 |
| internal/library | **46.6%** | 208 | 455 | 0 |
| internal/memory | 76.6% | 993 | 2242 | 377 |
| internal/meta | 79.9% | 399 | 860 | 0 |
| internal/models | 86.0% | 321 | 778 | 91 |
| internal/obs | 74.8% | 576 | 1290 | 135 |
| internal/otlp | 88.2% | 593 | 1338 | 397 |
| internal/placement | 90.1% | 232 | 554 | 390 |
| internal/root | 63.3% | 330 | 690 | 81 |
| internal/runtime | 74.3% | 2394 | 5229 | 2350 |
| internal/service | **41.7%** | 496 | 1007 | 105 |
| internal/store | 78.9% | 402 | 1073 | 293 |
| internal/toml | 77.1% | 362 | 646 | 75 |
| internal/video | 77.6% | 500 | 1053 | 219 |
| internal/voices | 91.0% | 357 | 723 | 368 |
| internal/web | 70.9% | 2456 | 5195 | 2770 |
| **Total** | **73.7%** | 25390 | | |

## 3. Behaviours and their guards

### Memory admission

| Behaviour | Guard | Status |
|---|---|---|
| Reserve admits or rejects; permanent shortfall | `TestReserveAdmitsAndRejects`, `TestPermanentShortfall` | Guarded |
| Demand figures match TS for every model | `TestPlanMatchesTypeScript`, `TestCatalogMemoryMatchesTypeScript`, `TestGeometryMatchesTypeScript` | Guarded |
| **Order: observed > measured > feed > estimate** | `ApplyObserved` is exercised, but `LLM.admissionDemand` is 52% covered: nothing fails if the order changes | **GAP** |
| Media demand and threshold | `TestMediaAdmissionGolden`, `TestServiceMissingCatalogImageChecksFitFirst` | Guarded |
| Switch preflight, footprint credit, rollback | `TestSwitchPreflightCreditCappedAtFootprint`, `TestPreflightRejectionKeepsCurrent`, `TestSwitchFailureRestoresPrevious`, `TestPostStopRollback`, `TestSwitchPreflightRejectedEvent` | Guarded |
| GPU working-set cap | `TestGPUWorkingSetSum` | Guarded |
| Unmeasurable memory rejects | `TestUnavailableMeasurementRejects` | Guarded |
| Placement planner, startup check | `TestGptossGolden`, `TestDensePlacement`, `TestUnifiedPlacement`, `TestCheck*`, `TestPlacementMismatchFailsClosed`, `TestPlacementGPUOnlyNeverFallsBack` | Guarded |
| `gpu:0` pool | `TestGPUPool`, `TestPlacementGPUPool`, `TestAdmissionRejectionNamesPoolWithNVIDIA` | Guarded with fakes; **never on real NVIDIA** |
| Fit-aware default model | `TestDefault*`, `TestExplicitRequestIsNeverSubstituted` | Guarded |

### Pressure, emergency stop, recovery

| Behaviour | Guard | Status |
|---|---|---|
| Thresholds and hysteresis | `TestHysteresis`, `TestMemoryPressureGolden` | Guarded |
| Pressure stops runtimes | `TestPressureStopsRuntimes`, `TestPressureFailsActiveJob` | Guarded |
| Emergency answers 503 or ends the stream | `TestEmergencyStopAnswers503`, `TestEmergencyStopEndsStream`, `TestMemoryEmergencyResponse`, `TestRunMemoryPressure` | Guarded |
| Idle recovery before refusing | `TestIdleRecoveryStopsIdlePeers`, `TestIdleRecoveryKeepsPeersWhenItCannotHelp`, `TestServiceClaimHoldsRequests`, `TestLLMClaimHoldsRequests` | Guarded |
| Speech/transcription interrupted by an emergency | `ServiceLease.Interrupted` is 0% | **GAP** |

### Queue, 429, Retry-After

| Behaviour | Guard | Status |
|---|---|---|
| FIFO across a switch; full and timeout | `TestFIFOAcrossSwitch`, `TestQueueFullAndTimeout` | Guarded |
| Status and `Retry-After` mapping | `TestRuntimeErrorRetrySemantics`; `TestDifferentialRetrySemantics` **R** | Guarded |
| Cancelling while queued (LLM) | `TestRunCancel` | Guarded |
| Cancelling while queued (media services) | `Service.remove`, `abandon` 0% | **GAP** |

### Runtime supervision

| Behaviour | Guard | Status |
|---|---|---|
| Cold start, switch, serialize, recover | `TestColdStartAndSwitch`, `TestServiceSwitchSerializeAndRecover` | Guarded |
| Crash: restart on next request | `TestCrashedBackendRestartsOnNextRequest` | Guarded |
| Stop events and reasons | `TestStopEvents`, `TestUnloadIdle` | Guarded |
| Explicit unload and drain | `TestUnload*` | Guarded |
| Pinned runtime install | `TestEnsureBinaryInstallsPinnedRelease`; `extractZip` 0% | Partial |
| Video jobs | `TestJob*`, `TestVideoJobLifecycle`, `TestFailedStopRetries`; `TestVideoReal` **R** | Guarded |
| CrispASR launch, voices, marking | `crispasrCommand`, `Voices`, `SpeechMarking` 0%; `TestSpeechKokoroCrispASR` **R**, needs weights | **GAP** |
| Linux GPU contract checks | `requireSdGPUContract` 0%; Linux host memory only runs in `test-linux` | Partial |
| Custom backend flag probe | `hasFlag`, `customDemand` 0% | **GAP** |

### Authentication

| Behaviour | Guard | Status |
|---|---|---|
| API keys and scopes | `TestAuthentication`, `TestRunOwnership`, `TestUnloadScope`; `TestCLIInitAndKeys` **D** | Guarded |
| OIDC, PKCE, nonce | `TestOIDCSignIn`, `TestJWTAlgorithms` | Guarded |
| **Cloudflare Access assertion** | `Browser.Authenticate` is 32% covered: the `Cf-Access-Jwt-Assertion` path, including its service-token and audience refusals, has no test | **GAP** |
| Magic link | `TestMagicLinkSignIn`; `MagicLinkPage`, `RevokeMagicLink` 0% | Partial |
| Sessions compatible with TS | `TestSessionRowsMatchTS` | Guarded |
| CSRF and same-origin | `TestCSRF`, `TestSameOrigin` | Guarded |
| **Cookie-only event stream** | `SameOriginStream` 0% (mine, from #384) | **GAP** |
| Lockout rules | `TestAccessLockout`, `TestKeyLockout`; `TestCLIAccessRecovery` | Guarded |
| Audit events, ids only | `TestAdminAuditRedaction` | Guarded |
| `--no-auth` loopback only | #405 | Guarded once #405 lands |

### Tokens and private headers

| Behaviour | Guard | Status |
|---|---|---|
| HF token scope, never on CDN origins | `TestHFTokenCDNScope`, `TestRedirectDowngradeRefused` | Guarded |
| URL redaction | `TestRedactURL` | Guarded |
| HF token validation | `CheckHFToken` 0% | **GAP** |
| Secrets stay out of bundles | `TestCLIDiagnostics` **D**, planted secrets | Guarded, by hand only |

### Catalog, discovery, install

| Behaviour | Guard | Status |
|---|---|---|
| Built-in catalog pinned and generated | `TestBuiltInCatalogDecodesStrictly`, `TestBuiltInCatalogMatchesSource` | Guarded |
| User `catalog.d`, former entries | `TestUserLayer`, `TestFormer*`, `TestUserFileThatPins*`, and others | Guarded |
| Remote feed and annotations | `TestParseFeedGolden`, `TestParseAnnotationsGolden`, `TestFeedSchedulerReloadsCatalog`; `TestCLIFeed` **D** | Guarded |
| Discovery | `TestDiscoveryGolden`, `TestModelDiscoveryEndToEnd`; `discoveryInstall` 0% | Partial |
| Install consent | `TestDifferentialModelManagement` | Guarded |
| Interrupted installs | `TestInstallInterruptedByGatewayStop` | Guarded |
| **Model download: checksum, framing, resume** | `internal/install` `download`, `checkFraming`, `DownloadURL` 0%, with no test file | **GAP** |

### Model import and backend selection

| Behaviour | Guard | Status |
|---|---|---|
| Hugging Face, URL, local, resume | `TestHuggingFaceImport`, `TestDirectURLImport`, `TestResumeAndLocal` | Guarded |
| Selection rules | `TestSelectionModes`, `TestCustomDefinitionRules`, `TestNoRatingFromNames`, and others | Guarded |
| End to end | `TestModelImportAcceptance` **D** | Guarded, by hand only |
| **HTTP APIs: backends, model-backend, imports, auth-profiles** | `backendsAPI`, `modelBackendAPI`, `modelImportsAPI`, `authProfilesAPI` 0%; `library` 46.6% | **GAP** |

### Detached runs

| Behaviour | Guard | Status |
|---|---|---|
| Survives disconnect, resume, cancel, ownership | `TestRunSurvivesDisconnect`, `TestRunEventsResume`, `TestRunCancel`, `TestRunOwnership` | Guarded |
| Restart sweep, expiry, plaintext rules | `TestRunRestartInterrupted`, `TestRunExpiry`, `TestRunCheckpointsStayOutOfBundles` | Guarded |
| Active and output limits | `TestRunLimits` | Guarded |
| Retained-run limit, max duration | `trim` 30%; `max_duration` never fires in a test | **GAP** |
| Lifecycle logs | `TestRunLogs` uses a plain logger; `TestSpeechTruncationInRun` covers the real one for one event | Partial |

### Speech, transcription, image

| Behaviour | Guard | Status |
|---|---|---|
| Speech validation and truncation flag | `TestDifferentialSpeechRejections`, `TestSpeechTruncation*` | Guarded |
| Image validation, seed echo | `TestDifferentialImageRejections`, `TestImageSeed*`, `TestValidatePNG`, `TestValidateJPEG` | Guarded |
| **Transcription response normalization** | `normalizeTranscriptionJSON`, `validTranscription` run only in `TestTranscriptionReal` **R**: no fake whisper | **GAP** |

### Observability

| Behaviour | Guard | Status |
|---|---|---|
| JSONL redaction | `TestEventsGolden`, `TestMetricAttributes`, `TestAdminAuditRedaction` | Guarded |
| **JSONL rotation** | `rotate` 0% | **GAP** |
| **Service stderr bound** | `BoundServiceStderr` 0%; `InspectServiceStderr` via `TestCLIDiagnostics` **D** | **GAP** |
| Logs CLI | `TestCLILogs` **D**, `TestTSReadsGoLogs` **D**; `Follow`, `Human` 0% | Partial |
| OTLP | `TestExportToCollector`, `TestRatioSamplerGolden`, `TestShutdownWithHungCollector`, and others; `TestDifferentialOTLP` **D**; `TestRealCollector` **R** | Guarded |
| Diagnostics bundle | `TestCLIDiagnostics` **D**, `TestRunCheckpointsStayOutOfBundles` | Guarded, mostly by hand |
| Error kinds on requests | `TestHTTPRequestLogsErrorKind` | Guarded |

### Service management

| Behaviour | Guard | Status |
|---|---|---|
| Rendered launchd and systemd units | `TestUnitsMatchTS`, `TestRenderSystemd`, `TestParseLaunchctl`, `TestParseLaunchdEnabled` | Guarded |
| **Start, stop, restart, remove, including restart rewriting the plist** | `Start`, `Stop`, `RestartWithin`, `write`, `enable`, `disable`, `Remove`, `WaitReady` 0%. Only the manual `tools/rehearsal.sh`, `tools/systemd-check.sh` | **GAP** |
| Gateway lock | `TestOperationLease`; `root.Lock`, `ClearStale` 0%, `LockAs` 42% | **GAP** |

### Configuration and database

| Behaviour | Guard | Status |
|---|---|---|
| Declarative TOML | `TestDeclarativeGolden`, `TestMatchesBun`, `TestKeepFormerSelections`; `TestCLIConfigDeclarative` **D** | Guarded |
| Drizzle journal compatible | `TestEmbeddedMigrationsMatchDrizzle`, `TestFreshDatabaseJournalIsDrizzleCompatible`, `TestOpensTypeScriptDatabase` | Guarded |
| `go_schema` stays 1; `model_placements` | `TestGoSchemaVersionForRollback`, `TestPlacementStore` | Guarded |
| Rollback drill | `tools/placement-rollback-check.sh`, by hand | **GAP in CI** |

### CLI and version

| Behaviour | Guard | Status |
|---|---|---|
| Argument parsing, help | `TestParsing`, `TestInputErrors`, `TestHelpAndVersion` | Guarded |
| Commands match TS | `TestCLI*` **D** | Guarded, by hand only |
| **`smoke`** | `TestCLISmoke` **D** runs, but no function in `smoke.go` is reached without runtimes | **GAP** |
| Command list stable | nothing fails in CI if a command disappears | **GAP** |
| Version stamped | #406 | Guarded once #406 lands |

## 4. Characterization tests to add first

In this order, each before refactoring its area. All use fakes; none need real models.

| # | Tests | Closes |
|---|---|---|
| 1 | Admission order: a table of observed, measured, feed and estimate through `admissionDemand` | the order |
| 2 | Cloudflare Access: fake JWKS; good assertion, wrong audience, service token, non-identity | Authenticate |
| 3 | `SameOriginStream`, `MagicLinkPage`, `RevokeMagicLink` | browser edges |
| 4 | JSONL rotation; service stderr bound | observability |
| 5 | `internal/install` against `httptest`: checksum, short body, HTML error page, resume | downloads |
| 6 | A fake `launchctl`/`systemctl` on PATH: start, stop, restart rewriting the plist, remove; lock and stale lock | service |
| 7 | HTTP tests for backends, model-backend, imports, auth-profiles | import APIs |
| 8 | A fake whisper: transcription normalization, `verbose_json`, formats | transcription |
| 9 | Media queue cancel; `Interrupted`; retained-run trim; max duration (with an injected clock) | runtime and runs |
| 10 | `smoke` against the fake backend; a CLI command-list snapshot | CLI |

## 5. Cleanup targets

Measured. Nothing here is done yet. `internal/web` is excluded (the FE thread owns it).

| Target | Evidence | Plan |
|---|---|---|
| Dead code | `deadcode`: 12 functions. `IsUIAccessPath`, `jsonUnmarshal` ×2, `sortedKeys`, `schemaRejection.Error`, `OverrideGPUMemory`, `obs.Redact`, `buf.int64`, `buf.boolean`, `placement.LastLines`, `toml.Quote`, `toml.Value`, `cli.SortedKeys`. Plus `archiveRe` (U1000) | Delete, one PR |
| Staticcheck findings | SA4010 `auth/policy.go:227` (result unused: **check, may be a bug**), SA4006 `video_contract.go:316`, SA4015 `plan.go:397`, SA4004 `runtime/service.go:285`, SA1019 `auth/jwt.go:78-79` (deprecated `ecdsa` fields, 5 sites) | Review each. ST1005 (165, capitalized errors) excluded: they are product messages |
| Oversized functions | `access` command 517 lines, `manage.go:294` 322, `serve` 297, `configure` 239, `imageEdit` 204, `hub/entry.go:500` 197, `gguf.go:444` 188, `schema.go:207` 186, `transcription` 184, `resolve.go:242` 176 | Split along real steps, each after its tests |
| Oversized files | `gateway/runs.go` 1082, `service/service.go` 1007, `gateway/manage.go` 943, `hub/entry.go` 886, `runtime/llm.go` 843 | Split by responsibility |
| Duplicate helpers | `contains` ×5, `writePrivate` ×3, `jsNumber` ×3, `fileExists` ×2, `jsonUnmarshal` ×2, `localeCompare` ×2, `number` ×2, `utf16Len` ×2, `nowISO` ×2, `uuid` ×2 | `slices.Contains`; a small `internal/fsutil` |
| Error envelopes | `writeError` plus hand-written: `gateway/manage.go:47,120`, `unload.go:59`, `relay.go:621,630`, `auth.Deny`. Some are TS's exact shape | One helper with an extras field; keep the TS shapes |
| Log attributes | `model_id` ×23, `model` ×12, `modelId` ×4; `durationMs`, `duration_ms`. 6 log calls have no `event` | Settle on `model_id`, `duration_ms`. **Log names can be a contract: check `TestTSReadsGoLogs` first** |
| Config and env access | About 20 `LOCALBASE_*` variables read directly across packages | One place, with a tested reference |
| Context | 109 `context.Background()` outside tests | Thread the caller's where one exists; many are legitimate |
| Goroutines | 21 `go func` sites; `LLM.watch` isn't waited on by `Close`; **no leak check anywhere** | Own each; add a goleak-style check per package. **Stdlib only: the project keeps to one module dependency, so this will be a small helper, not a new module** |
| Test helpers | `installSparse` ×3, `loadGolden` ×3, `testStore` ×2, plus `freePort`, `waitFor` | `internal/testutil` |
| Fixtures | `gateway/testdata/video.json` 6.3 MB | Generate or trim |
| Stale comments | `llm.go:551`: "feed: milestone 5", though feed is implemented; TODOs only in vendored `norm` | One PR |
| Package layering | No cycles. `runtime` imports `install`, `conf` and `hub` import `runtime`; `gateway` depends on 19 packages | Report only unless a split needs it |

### Flaky tests

| Test | Failure | Status |
|---|---|---|
| `TestUnloadScope` | once, temp-dir cleanup only; not reproduced | Recorded; `scratchDir` will explain the next one |
| `TestRunOwnership` | once under `-race`, at about 11:20 UTC on 2026-11-03: `status failed, want [completed]`, whole output in that line. It passed 5 of 5 and a full package run straight after | Recorded, cause unknown: the 10 s is `waitStatus` polling a run that had already failed, not a deadline on the run, and the helper didn't print why. It now stops at a finished status and prints the run's error, so the next one will say |
| `TestRuntimePortBaseServes` | once under `-race`, a 503 from `/v1/chat/completions`, at about 10:15-10:16 UTC on 2026-11-03; passed 5 of 5 straight after | Recorded. A test flood had exhausted the machine's local ports at about that time, which fits, but nothing proves it. Output kept by the Go thread until the next reboot |
| `TestEmbeddings`, `TestDifferentialModelMetadata`, `TestDifferentialImageRejections` | **D** or **R**: memory refusals while other models are loaded | Load-dependent; the image test logs free memory on failure |

## 6. TS-parity shims: keep until TS retires

Listed so they are not "cleaned up" early. About 100 source files mention TS.

| Shim | What for | After TS retires |
|---|---|---|
| `drizzle` journal, `store/migrations` | binary-swap rollback | Move to Go's own migrations |
| `go_schema` staying at 1 | old binaries still open the database | Normal versioning |
| `internal/jsjson` (16 importers) | JSON byte-identical to `JSON.stringify` | Keep where a format is public; else `encoding/json` |
| `internal/collate` | `localeCompare` order | Decide per use |
| `internal/third_party/norm` (9,935 lines) | voice names matching JS `normalize`; used only by `voices` | Likely removable with a one-time check of stored names |
| `jsNumber`, `utf16Len`, `toml` matching Bun | JS number and string semantics | Simplify |
| `tools/golden/*.ts` (20 files), `TestPlanMatchesTypeScript` and the other `*MatchesTypeScript`/`TestUnitsMatchTS` | generated TS expectations | Freeze goldens as Go's own, drop the generators |
| `contract/` TS target and differentials | the parity proof | Keep the suite, Go-only |
| Session rows and cookie names | sessions survive a switch | Free to change |
| Odd TS behaviours kept deliberately (for example no method check on transcription) | parity | Fix, as visible API changes needing approval |

Removal is a follow-up PR after retirement, not part of this program.

## 7. What Step 2 already has, and doesn't

| Gate | State |
|---|---|
| `go vet`, `-race`, gofmt with 1.26 | **Already in `go.yml`,** on macOS and Linux |
| golangci-lint | None. `~/go/bin` has 1.63.4 (built with Go 1.23), too old for this code: the pinned config needs a current release |
| Coverage report | `tools/coverage.sh`, this PR |
| Fuzz tests | **None** |
| Benchmarks | **None** |
| Route table or OpenAPI tested against routes | **None** |
| Config reference tested against code | **None** |
| Version stamping, reproducible build | #406 |
| Goroutine leak checks | **None** |

## Reproducing

```sh
go test ./... -count=1                          # what go.yml runs, plus -race there
LOCALBASE_CONTRACT_MODELS=<models/llm dir> tools/coverage.sh both
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/tools/cmd/deadcode@latest -test ./...
```
