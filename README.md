# LocalBase

LocalBase is a Bun/TypeScript unified, OpenAI-compatible gateway for local AI runtimes. It listens on port `2273` by default and manages model processes behind one API surface.

## Current capabilities

- **LLM** OpenAI-compatible chat completions, including configured-model switching.
- **Embeddings** for local indexing and search.
- **STT** audio transcriptions and translations.
- **Image generation** through the OpenAI-compatible `/v1/images/generations` endpoint.
- Lazy loading of LLM, STT, and image backends on first use.
- Self-healing process supervision with bounded restart backoff.
- Zod request and response validation.
- SQLite-backed configuration and API-key storage.
- Hardware-aware context sizing and llama-server parallel-slot configuration.

The runtime currently keeps one active model per service. Dynamic model pools and eviction are future work.

## Supported platforms

Full managed support includes the CLI and automatic backend management:

- macOS ARM64.
- Linux x64.

CLI-only compatibility is available for macOS x64 and Linux ARM64. These releases publish the Bun CLI but no LocalBase-built `whisper-server` or `sd-server` runtime. Put compatible backend executables in `$LOCALBASE_ROOT/bin` (by default `~/.local/share/local-base/bin`) or on `PATH`. Pinned upstream `llama.cpp` downloads remain available only where that upstream release provides them.

Windows is unsupported.

## Quick start

```bash
bun install
bun run build
./dist/local-base configure
./dist/local-base serve
```

The gateway is available at `http://localhost:2273/v1`. Use `./dist/local-base --help` for command details. API keys can be created with `./dist/local-base keys create`.

## Run as a user service

`serve` always runs in the foreground. `start` installs, enables, and starts a root-specific user service. `stop` stops and disables login startup without removing the definition. `restart` refreshes and enables the service. `status` reports manager state and gateway readiness without opening the LocalBase database.

```bash
./dist/local-base start
./dist/local-base status
./dist/local-base stop
```

macOS uses a launchd user agent and Linux uses a `systemd --user` service. `uninstall --yes` stops and removes the matching service before deleting that LocalBase root.

## Operational logs

`serve` is the single writer of redacted JSON Lines events under `$LOCALBASE_ROOT/logs`. The active file rotates at 10 MiB and retains five archives. These files are the primary operational record for foreground and managed services. A managed startup failure before the primary sink is available atomically records one private, bounded structured bootstrap event. launchd output is discarded; the systemd journal remains a secondary Linux fallback.

```bash
./dist/local-base logs --level error
./dist/local-base logs --limit 500 --since 2026-01-01T00:00:00Z
./dist/local-base logs --follow --runtime llm
./dist/local-base --json logs --request-id req-123
```

Finite `logs --json` calls return the normal JSON command envelope and default to the newest 200 matching events (maximum 5,000). `logs --follow --json` streams one validated log event per JSONL line to stdout. Log records redact credentials, cookies, secret URL values, request identifiers that resemble credentials, and request or model content before they reach any sink.

### OpenTelemetry export

Local JSONL remains authoritative. Version 2 records sampled trace correlation as one `trace` object. Setting an OTLP/HTTP endpoint enables bounded asynchronous export of logs and traces:

```bash
./dist/local-base configure --otel-endpoint http://localhost:4318 --otel-sample-ratio 25
```

Standard `OTEL_EXPORTER_OTLP_ENDPOINT`, signal-specific endpoint/header variables, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_TRACES_SAMPLER`, and `OTEL_TRACES_SAMPLER_ARG` override persisted settings. LocalBase uses W3C `traceparent` and `tracestate`, propagates valid request context to backends, and correlates local logs with sampled spans. Baggage, prompts, responses, credentials, and arbitrary headers are never exported. Collector outages can drop bounded telemetry but do not delay or fail inference; shutdown gives all telemetry signals one shared five-second flush deadline.

## Automation and JSON output

Use the global `--json` option for automation. It may appear before or after a command, but not after `--`.

```bash
local-base --json models catalog
local-base doctor --json
```

Finite commands write exactly one JSON document to stdout:

```json
{ "ok": true, "data": {} }
```

```json
{ "ok": false, "error": { "code": "invalid_input", "message": "..." } }
```

Diagnostics, progress, and errors are written to stderr. Exit codes are `0` for success, `1` for operational failures, and `2` for invalid input. `--json` disables interactive prompts; destructive commands still require `--yes`, and `configure` creates an API key only with explicit `--create-key`.

`serve --json` writes JSON Lines lifecycle events (`started`, `stopped`, and `error`) to stdout. Gateway logs remain on stderr; OpenAI-compatible HTTP and SSE responses are unchanged.

## Continue.dev integration

If `~/.continue/config.json` exists, `configure` and `serve` synchronize LocalBase model entries, autocomplete, embeddings, API base, and calculated context settings with that file.

## Development

Install dependencies and run the source CLI with Bun:

```bash
bun install --frozen-lockfile
bun run local-base --help
```

Useful verification commands:

```bash
bun run check
bun test
bun run build
```

`bun run check` formats-checks the source, type-checks the project, and runs the CLI help smoke test. `bun run build` produces `dist/local-base`.

Database changes use Drizzle. Run `bun run db:generate` to create SQL migrations and `bun run db:check` to validate the tracked SQL and journal. Install and build preparation generate the ignored asset module embedded by compiled CLIs.

## Contributing

Keep documentation and behavior aligned, use Bun for project commands, and verify changes with the checks above before opening a pull request. Report bugs and requests through [GitHub Issues](https://github.com/timsexperiments/LocalBase/issues).
