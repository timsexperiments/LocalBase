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

CLI-only compatibility is available for macOS x64 and Linux ARM64. These releases publish the Bun CLI but no LocalBase-built `whisper-server` or `sd-server` runtime. Put compatible user-managed backend executables on `PATH`, outside `$LOCALBASE_ROOT/bin` (by default `~/.local/share/local-base/bin`). Pinned upstream `llama.cpp` downloads remain available only where that upstream release provides them.

Linux managed-runtime releases are built and qualified against an Ubuntu 24.04-compatible userspace and require the GNU OpenMP runtime (`libgomp`, packaged as `libgomp1` on Ubuntu).

Managed runtime versions are pinned independently from LocalBase CLI releases.

## Getting started

### Download and verify

Download one archive and `checksums.txt` from [GitHub Releases](https://github.com/timsexperiments/LocalBase/releases):

- `local-base-macos-arm64.zip`
- `local-base-macos-x64.zip`
- `local-base-linux-x64.tar.gz`
- `local-base-linux-arm64.tar.gz`

In the download directory, set `ARCHIVE` to the downloaded archive and verify its checksum:

```bash
ARCHIVE=local-base-macos-arm64.zip
grep -F "  $ARCHIVE" checksums.txt > "$ARCHIVE.sha256"
shasum -a 256 -c "$ARCHIVE.sha256" # macOS
sha256sum -c "$ARCHIVE.sha256"    # Linux
```

Optionally verify the GitHub build attestation:

```bash
gh attestation verify "$ARCHIVE" --repo timsexperiments/LocalBase
```

### Install the CLI

Extract the archive, install the target-specific executable, and ensure `$HOME/.local/bin` is on `PATH`:

```bash
case "$ARCHIVE" in
  *.zip) unzip "$ARCHIVE"; CLI="${ARCHIVE%.zip}" ;;
  *.tar.gz) tar -xzf "$ARCHIVE"; CLI="${ARCHIVE%.tar.gz}" ;;
esac
mkdir -p "$HOME/.local/bin"
install -m 755 "$CLI" "$HOME/.local/bin/local-base"
export PATH="$HOME/.local/bin:$PATH"
local-base --help
```

Add `export PATH="$HOME/.local/bin:$PATH"` to the shell profile to retain the path in new shells.

Run the system check:

```bash
local-base doctor
```

### Configure and install a model

Configure an LLM-only gateway with an initial API key:

```bash
local-base --non-interactive configure --defaults \
  --llm-models qwen2.5-coder-1.5b-instruct-q4_k_m \
  --active-llm qwen2.5-coder-1.5b-instruct-q4_k_m \
  --stt-models '' \
  --image-models '' \
  --parallel auto \
  --create-key
```

The API key secret is displayed once. Store it securely.

Install the selected model before starting the service so download and checksum progress remain visible in the foreground:

```bash
local-base --non-interactive models install qwen2.5-coder-1.5b-instruct-q4_k_m
```

### Start and use the gateway

Start the user service and verify its state:

```bash
local-base start
local-base status
local-base models list
```

Set the API key created during configuration and send an OpenAI-compatible request:

```bash
export LOCALBASE_API_KEY='lb_...'

curl http://localhost:2273/v1/chat/completions \
  -H "Authorization: Bearer $LOCALBASE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "qwen2.5-coder-1.5b-instruct-q4_k_m",
    "messages": [{"role": "user", "content": "Say hello in two words."}]
  }'
```

## Operations

`serve` runs in the foreground. `start` installs, enables, and starts the user service. `restart` refreshes and enables it. `stop` stops it and disables login startup. `status` reports service-manager, process, gateway, and modality state without opening the LocalBase database.

macOS uses a launchd user agent. Linux uses a `systemd --user` service; detached commands require a functioning user manager. Foreground `local-base serve` does not.

```bash
local-base logs --follow
local-base restart
local-base stop
local-base diagnostics
local-base uninstall --yes
```

`uninstall --yes` stops and removes the matching user service before deleting that LocalBase root.

## Logs

`serve` is the single writer of redacted JSON Lines events under `$LOCALBASE_ROOT/logs`. The active file rotates at 10 MiB and retains five archives. These files are the primary operational record for foreground and managed services. A managed startup failure before the primary sink is available atomically records one private, bounded structured bootstrap event. launchd output is discarded; the systemd journal remains a secondary Linux fallback.

```bash
local-base logs --level error
local-base logs --limit 500 --since 2026-01-01T00:00:00Z
local-base logs --runtime llm
local-base --json logs --request-id req-123
```

Finite `logs --json` calls return the normal JSON command envelope and default to the newest 200 matching events (maximum 5,000). `logs --follow --json` streams one validated log event per JSONL line to stdout. Log records redact credentials, cookies, secret URL values, request identifiers that resemble credentials, and request or model content before they reach any sink.

### OpenTelemetry export

Local JSONL is the durable log record. Records with sampled span context include `trace: { traceId, spanId }`. An OTLP/HTTP endpoint enables bounded asynchronous log and trace export:

```bash
local-base configure --otel-endpoint http://localhost:4318 --otel-sample-ratio 25
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

Database changes use Drizzle. Run `bun run db:generate` to create SQL migrations and `bun run db:check` to validate the tracked SQL and journal. Installation and builds generate the ignored asset module embedded by compiled CLIs.

## Contributing

Keep documentation and behavior aligned, use Bun for project commands, and verify changes with the checks above before opening a pull request. Report bugs and requests through [GitHub Issues](https://github.com/timsexperiments/LocalBase/issues).
