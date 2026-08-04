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

## Setup

### Select a release archive

Use an immutable GitHub Releases tag that contains the current archive set. Do not use a moving `latest` URL.

| Host        | Archive                         | Support tier | Runtime requirements                                                                                                    |
| ----------- | ------------------------------- | ------------ | ----------------------------------------------------------------------------------------------------------------------- |
| macOS ARM64 | `local-base-macos-arm64.zip`    | Managed      | LocalBase manages `llama-server`, `whisper-server`, and `sd-server`.                                                    |
| macOS x64   | `local-base-macos-x64.zip`      | CLI-only     | LocalBase can install pinned `llama-server`; provide compatible `whisper-server` and `sd-server` executables on `PATH`. |
| Linux x64   | `local-base-linux-x64.tar.gz`   | Managed      | Ubuntu 24.04-compatible userspace and `libgomp1` are required.                                                          |
| Linux ARM64 | `local-base-linux-arm64.tar.gz` | CLI-only     | LocalBase can install pinned `llama-server`; provide compatible `whisper-server` and `sd-server` executables on `PATH`. |

CLI-only user-managed runtimes must be outside `$LOCALBASE_ROOT/bin` and are not verified by LocalBase. Managed Linux runtimes are qualified against an Ubuntu 24.04-compatible userspace. Windows is unsupported.

### Download and verify

Replace `vX.Y.Z` with an immutable tag shown on [GitHub Releases](https://github.com/timsexperiments/LocalBase/releases). Do not use `latest` or another moving reference. Then download the archive and checksum manifest:

```bash
RELEASE_TAG=vX.Y.Z # replace with an immutable GitHub Releases tag
ARCHIVE=local-base-macos-arm64.zip # select the archive for this host
BASE_URL="https://github.com/timsexperiments/LocalBase/releases/download/$RELEASE_TAG"
curl -fLO "$BASE_URL/$ARCHIVE"
curl -fLO "$BASE_URL/checksums.txt"
grep -F "  $ARCHIVE" checksums.txt > "$ARCHIVE.sha256"
test -s "$ARCHIVE.sha256"
```

Verify the checksum before extraction:

```bash
shasum -a 256 -c "$ARCHIVE.sha256" # macOS
sha256sum -c "$ARCHIVE.sha256"     # Linux
```

GitHub build attestations are published for each canonical archive and `checksums.txt`. Verify the downloaded subjects with the GitHub CLI:

```bash
gh attestation verify "$ARCHIVE" --repo timsexperiments/LocalBase
gh attestation verify checksums.txt --repo timsexperiments/LocalBase
```

macOS archives contain a signed and notarized executable. After extraction, verify its embedded signature:

```bash
MACOS_CLI="${ARCHIVE%.zip}"
codesign --verify --strict --verbose=2 "$MACOS_CLI"
spctl --assess --type execute "$MACOS_CLI"
```

No detached signature file is published for Linux archives.

### Install the CLI

Extract the archive and install the executable as `local-base`. `$HOME/.local/bin` is the recommended non-root default; set `INSTALL_DIR` to another absolute directory when needed:

```bash
case "$ARCHIVE" in
  local-base-macos-*.zip)
    unzip -q "$ARCHIVE"
    CLI="${ARCHIVE%.zip}"
    ;;
  local-base-linux-*.tar.gz)
    tar -xzf "$ARCHIVE"
    CLI="${ARCHIVE%.tar.gz}"
    ;;
  *)
    echo "Unsupported archive: $ARCHIVE" >&2
    exit 1
    ;;
esac
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
install -m 755 "$CLI" "$INSTALL_DIR/local-base"
```

Add the selected install directory to the current shell only when it is absent:

```bash
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) export PATH="$INSTALL_DIR:$PATH" ;;
esac
command -v local-base
local-base --help
```

If the directory is not already in `PATH`, persist it by running the command for the active shell. These commands intentionally edit the selected shell startup file for the recommended `$HOME/.local/bin` directory:

```bash
# zsh
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' "$HOME/.zshrc" 2>/dev/null || printf '%s\n' 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.zshrc"
source "$HOME/.zshrc"

# bash
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' "$HOME/.bashrc" 2>/dev/null || printf '%s\n' 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"
source "$HOME/.bashrc"
```

For a non-default `INSTALL_DIR`, replace `$HOME/.local/bin` in the shell configuration command with the selected directory.

### Initialize, configure, and install models

Initialize the data directory and inspect hardware:

```bash
local-base init
local-base doctor
```

List the catalog before choosing models:

```bash
local-base models catalog --kind llm
local-base models catalog --kind stt
local-base models catalog --kind image
```

Configure one model from each supported category when the host has sufficient resources. The example uses the smallest catalog entries in each category and creates an API key whose secret is displayed once:

```bash
local-base --non-interactive configure --defaults \
  --llm-models qwen2.5-coder-1.5b-instruct-q4_k_m \
  --active-llm qwen2.5-coder-1.5b-instruct-q4_k_m \
  --stt-models whisper-tiny-en-q8_0 \
  --active-stt whisper-tiny-en-q8_0 \
  --image-models stable-diffusion-v1-5 \
  --active-image stable-diffusion-v1-5 \
  --parallel auto \
  --create-key
```

Store the displayed API key securely. Use `local-base configure --all` for interactive configuration, or `local-base keys create --name default` to create another key.

Install selected models in the foreground so download and checksum progress remain visible:

```bash
local-base --non-interactive models install qwen2.5-coder-1.5b-instruct-q4_k_m
local-base --non-interactive models install whisper-tiny-en-q8_0
local-base --non-interactive models install stable-diffusion-v1-5
local-base models list
```

### Start and verify inference

Start the detached user service and inspect its state:

```bash
local-base start
local-base status
```

Set the API key and send an authenticated OpenAI-compatible request:

```bash
export LOCALBASE_API_KEY='lb_...'

curl http://127.0.0.1:2273/v1/chat/completions \
  -H "Authorization: Bearer $LOCALBASE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "qwen2.5-coder-1.5b-instruct-q4_k_m",
    "messages": [{"role": "user", "content": "Say hello in two words."}]
  }'
```

`local-base serve` runs the gateway in the foreground. `start` installs, enables, and starts the macOS launchd or Linux `systemd --user` service. `stop` stops and disables the user service. `restart` refreshes and restarts it.

```bash
local-base logs --follow
local-base diagnostics --output local-base-diagnostics.zip
local-base stop
local-base uninstall --yes
```

`uninstall --yes` stops and removes the matching user service before deleting the LocalBase data root.

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
