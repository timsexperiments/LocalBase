# LocalBase

LocalBase is a Bun/TypeScript unified, OpenAI-compatible gateway for local AI runtimes. It listens on port `2273` by default and manages model processes behind one API surface.

## Current capabilities

- **LLM** OpenAI-compatible chat completions, including configured-model switching.
- **Embeddings** for local indexing and search.
- **STT** audio transcriptions and translations.
- **TTS** through `/v1/audio/speech` with the bounded Qwen3 TTS base model.
- **Image generation** through the OpenAI-compatible `/v1/images/generations` endpoint.
- Lazy preparation of LLM, STT, TTS, and image runtimes on first use.
- Self-healing process supervision with bounded restart backoff.
- Zod request and response validation.
- SQLite-backed configuration and API-key storage.
- Hardware-aware context sizing and llama-server parallel-slot configuration.

The runtime keeps one active model per service. Memory pressure can evict idle runtimes or stop all runtimes in an emergency. Simultaneous multi-model process pools are not supported.

### Model metadata

Authenticated `GET /_localbase/models` and `GET /_localbase/models/:modelId` return catalog identity, every declared artifact checksum and size, catalog memory estimates, and local selection, installation, and runtime state. `selected` reports configured selection. `runtime` reports the observed applied supervisor, so a replacement may be selected while the previous model drains. The gateway captures both before file inspection. Runtime metadata separates resolved native slots, active admissions, arithmetic availability, immediate dispatch eligibility, and bounded waiting-room capacity. Reads do not start runtimes or hash model files. Capability fields remain `null` without an authoritative value; the TTS model reports its WAV, catalog-voice, and cold-request contract.

TTS is disabled by default. The first supported model is `qwen3-tts-1.7b-base-q4_k_m`. Requests must explicitly set `voice` to `default`, `harbor`, or `willow`, and `response_format: "wav"`; `default` uses no reference file. Harbor and Willow are fixed, checksum-pinned CC0 references from Kyutai's verified Unmute voice donations ([provenance](https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations)); clients cannot supply audio paths, URLs, or uploads. Omitted formats do not fall back from OpenAI's MP3 default. Speed is fixed at `1`, instructions are unsupported, and input is limited to 256 characters. Each request runs a cold, bounded native generation and returns PCM16 mono WAV at 24 kHz. Human voice quality has not been assessed.

Use [`local-base config`](docs/declarative-config.md) to validate, plan, apply, and export versioned TOML configuration without prompts.

Public `GET` and `HEAD /health/ready` report whether at least one configured modality can admit a request, including bounded queue waiting. `/health` remains process liveness.

## Supported platforms

Full managed support includes the CLI and automatic backend management:

- macOS ARM64.
- Linux x64.

CLI-only compatibility is available for macOS x64 and Linux ARM64. These releases publish the Bun CLI only; LocalBase does not download, install, or verify backend runtimes. Put compatible user-managed backend executables on `PATH`, outside `$LOCALBASE_ROOT/bin` (by default `~/.local/share/local-base/bin`).

Linux managed-runtime releases are built and qualified against an Ubuntu 24.04-compatible userspace and require the GNU OpenMP runtime (`libgomp`, packaged as `libgomp1` on Ubuntu).

Linux STT requires one monitored NVIDIA GPU and a PCI-capable LocalBase Whisper runtime. The runtime matches the admitted NVML device's PCI address to a unique discrete Vulkan backend, including on hosts with an Intel integrated GPU. Missing or ambiguous identity and GPU initialization failure stop STT; `--bypass-memory-check` does not enable CPU-only fallback. Older user-managed Whisper binaries are unsupported. This is not multi-discrete-GPU support. macOS keeps its Metal runtime.

Managed runtime versions are pinned independently from LocalBase CLI releases.

## Getting started

Download the archive for the host from an immutable tag on [GitHub Releases](https://github.com/timsexperiments/LocalBase/releases), along with `checksums.txt`.

```bash
ARCHIVE=local-base-macos-arm64.zip
grep -F "  $ARCHIVE" checksums.txt > "$ARCHIVE.sha256"
shasum -a 256 -c "$ARCHIVE.sha256" # macOS
sha256sum -c "$ARCHIVE.sha256"     # Linux
```

Extract and install the CLI:

```bash
unzip "$ARCHIVE"                    # macOS
# tar -xzf "$ARCHIVE"              # Linux
mkdir -p "$HOME/.local/bin"
install -m 755 "${ARCHIVE%.zip}" "$HOME/.local/bin/local-base"
export PATH="$HOME/.local/bin:$PATH"
```

For Linux archives, install `"${ARCHIVE%.tar.gz}"` instead. Add the `PATH` export to the active shell profile to make it permanent.

Configure a small LLM and create an API key:

```bash
local-base init
local-base --non-interactive configure --defaults \
  --llm-models qwen2.5-coder-1.5b-instruct-q4_k_m \
  --active-llm qwen2.5-coder-1.5b-instruct-q4_k_m \
  --stt-models '' \
  --tts-models '' \
  --image-models '' \
  --parallel auto \
  --create-key
local-base models install qwen2.5-coder-1.5b-instruct-q4_k_m
local-base start
```

Store the displayed API key, then verify inference:

```bash
export LOCALBASE_API_KEY='lb_...'
curl http://127.0.0.1:2273/v1/chat/completions \
  -H "Authorization: Bearer $LOCALBASE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-coder-1.5b-instruct-q4_k_m","messages":[{"role":"user","content":"Say hello in two words."}]}'
```

Use `local-base status` to inspect the service and `local-base logs --follow` to stream logs.

### Browser playground

See [Authentication and authorization](docs/authentication.md) for the trust
model, CI setup, identity-provider choices, access policies, API-key scopes,
and recovery procedures.

Configure the human identity provider, then restart LocalBase:

```bash
local-base access cloudflare \
  --team-domain your-team.cloudflareaccess.com \
  --audience YOUR_ACCESS_AUD_TAG \
  --origin https://localbase.example.com
local-base restart
```

Direct OpenID Connect uses Authorization Code with PKCE. Register
`https://localbase.example.com/oidc/callback` as the exact redirect URI. Read a
confidential client's secret from the environment so it never appears in the
process arguments:

```bash
export LOCALBASE_OIDC_SECRET='...'
local-base access oidc add \
  --id company \
  --name "Company sign-in" \
  --issuer https://identity.example.com \
  --client-id YOUR_CLIENT_ID \
  --client-secret-env LOCALBASE_OIDC_SECRET \
  --origin https://localbase.example.com
local-base restart
```

Use `--public-client` instead of `--client-secret-env` only when the provider
registration is explicitly a public client.

GitHub uses OAuth rather than OpenID Connect. Register
`https://localbase.example.com/github/callback` as the OAuth app callback URL:

```bash
export LOCALBASE_GITHUB_SECRET='...'
local-base access github add \
  --id github \
  --name GitHub \
  --client-id YOUR_GITHUB_CLIENT_ID \
  --client-secret-env LOCALBASE_GITHUB_SECRET \
  --origin https://localbase.example.com
local-base restart
```

OIDC and GitHub registrations can coexist on the same sign-in page. The first
registration receives the default browser permissions. Later additions preserve
the configured permissions unless `--permissions` is supplied. Use the matching
`access oidc` or `access github` `list` and `remove` commands. Removing the last
registration disables browser access.

Use `local-base access show --json` to inspect the active non-secret configuration and `local-base access disable` to disable browser sign-in. The default browser permissions allow chat and model discovery. Grant other inference permissions and `models:manage` explicitly. Pass `--permissions` to replace the defaults. Access configuration is restart-scoped and every command supports `--non-interactive` and `--json`.

An optional local policy replaces provider-wide permissions with named roles and
identity bindings. Subject bindings compare the exact issuer and opaque subject.
Email and email-domain bindings use only provider-verified email addresses. A
policy grants no permissions when no binding matches and must retain at least
one binding whose role grants `access:manage`.

```json
{
  "roles": {
    "admin": ["access:manage", "models:read", "models:manage"],
    "user": ["inference:chat", "models:read"]
  },
  "bindings": [
    {
      "role": "admin",
      "match": {
        "kind": "subject",
        "issuer": "https://identity.example.com",
        "subject": "exact-provider-subject"
      }
    },
    {
      "role": "user",
      "match": { "kind": "email-domain", "domain": "example.com" }
    }
  ]
}
```

```bash
local-base access policy apply --file access-policy.json --json
local-base access policy show --json
local-base access policy test \
  --issuer https://identity.example.com \
  --subject exact-provider-subject \
  --email person@example.com \
  --json
local-base restart
```

`access policy test` treats `--email` as verified input for offline evaluation.
`access policy clear` restores the provider-wide permissions configured by
`access cloudflare`, `access oidc`, or `access github`. Applying and clearing a policy require a
restart.

Open `/app` on the configured HTTPS origin. The playground requires a verified human session from Cloudflare Access, OpenID Connect, or GitHub; gateway API keys cannot authenticate the browser UI. Direct sessions are opaque, HTTP-only cookies and are cleared when LocalBase restarts. Chat streams normal LLM responses. Models with the `tool-calling` feature can call `generate_image`, `generate_video`, and `synthesize_speech` when the corresponding models are selected and installed. The browser validates tool arguments, runs tools sequentially, and limits each turn to four model rounds and four tool calls. Text summaries and tool-call IDs continue the conversation; generated media bytes and download URLs never enter model context.

Open **Access & API keys** from Settings or `/app?panel=admin` to inspect and update identity-provider configuration, access policies, API-key scopes, rotation, and revocation. The page appears only after human sign-in and each operation still requires its corresponding `access:*` or `keys:*` permission. New and rotated API-key secrets are held only in page memory and shown once.

Model Lab calls models directly without generation tools. It supports chat, images, speech, audio-file transcription, text-to-video, and embeddings. Voice choices, embedding dimension bounds, and the fixed video profile come from model metadata. Completed videos play inline and download as MP4 in Chat and Model Lab. Speech-to-video portrait and audio inputs are not supported in Model Lab yet.

The gateway converts the runtime's AVI output to MP4 using a pinned, checksum-verified converter installed automatically when needed. It does not use a system FFmpeg installation.

Stop aborts inference and media requests. Video submission is allowed to return its job ID before cancellation so the browser can cancel and delete the job with the same verified identity. Cleanup failures appear as warnings without discarding completed downloads. The static shell is public, while model metadata and inference require the configured browser identity provider. Runtime admission and resource limits remain enforced by the gateway.

History lasts for the open page by default. Settings can opt into device-local text history; credentials, attachment payloads, generated media, and tool protocol messages are never stored. Saved conversations with missing attachments require a new conversation before sending. The UI uses no service worker.

Chat accepts up to four UTF-8 text or code attachments, limited to 128 KiB each and 256 KiB combined. Models declaring image input also accept PNG, JPEG, and WebP images up to 5 MiB each. Attachments accompany follow-ups and retries for the open page. PDF, Office documents, and archives are not supported. Managed chat models currently declare text input only.

Microphone buttons dictate into text fields using the installed, enabled STT model selected in Settings. Tap once to record and again to transcribe; text is added without submitting. Recordings are limited to one minute, converted to mono 16 kHz WAV in the browser, and sent to the gateway's transcription endpoint. Navigating away or cancelling discards the recording. Microphone access requires browser permission and HTTPS or localhost; plain-HTTP LAN addresses cannot record. Password, numeric, and file inputs do not offer dictation.

Open **Manage models** from the model picker or `/app?panel=catalog` to browse the catalog, disk usage, and download sizes. Only installed, enabled models can be selected for inference. Installation downloads files; enabling makes them available for requests; setting a default chooses the model used when a request omits one. Disable a model and wait for its runtime to release it before uninstalling. Shared model files are retained.

Model administration requires `models:manage`; catalog reads require `models:read`. Browser sessions receive the permissions configured by `local-base access cloudflare`, `local-base access oidc`, or `local-base access github`. Machine clients can receive the same capability with `local-base keys create --scopes models:read,models:manage`. Ordinary client keys retain inference and model-read access by default.

`GET /_localbase/model-management` returns catalog installation state, storage, operation progress, and `canManage`. Authorized clients use `POST` on the same endpoint with `{"modelId":"<catalog-id>","action":"install"}`. Actions are `install`, `uninstall`, `enable`, `disable`, and `activate`. Install returns HTTP 202; poll GET for completion or failure. One installation runs at a time, and progress is retained only for the running gateway process. Other actions return HTTP 200 when complete.

Authentication administration uses two exact, always-authenticated endpoints:

- `GET` and `POST /_localbase/access-management` require `access:read` for reads and policy tests, or `access:manage` for provider and policy changes.
- `GET /_localbase/api-keys` requires `keys:read`. `POST` requires `keys:manage` and accepts `create`, `rotate`, `revoke`, or `set-scopes` actions.

Provider responses never include client secrets. API-key creation and rotation return the new key secret once; reads and later mutations return metadata only. Access provider and policy changes report `restartRequired`; key changes take effect on the next request. These permissions are not granted to browser sessions or ordinary API keys automatically. Grant them explicitly with `local-base access ... --permissions`, an access policy, or `local-base keys create --scopes ...`. The CLI remains the primary recovery and automation interface because it works directly against the local data directory.

Models sharing files with an installation must be disabled and released by their runtimes. Do not run concurrent CLI installs or re-enable affected models through the CLI while an API installation is running.

`bun install`, `bun run db:prepare`, and release builds prepare the browser assets. After UI edits, run `bun run db:prepare` for source execution or `bun run build` for a standalone CLI. Compiled binaries embed the assets and need no checkout or runtime build.

## Structured outputs

Chat completions accept OpenAI-compatible `response_format.type: "json_schema"` requests. LocalBase compiles the schema before starting a model and forwards the response format unchanged to the managed llama runtime. Schemas must use a root object, require every declared property, set `additionalProperties: false`, and fit within 10 nesting levels, 5,000 total properties, 1,000 local references, and 256 KiB. LocalBase also limits each object to 1,000 properties as a synchronous compilation resource bound; this is distinct from OpenAI's overall property limit.

The supported contract includes primitive and nullable types, strict object properties, object-valued array items, array bounds and string maximum lengths up to 10,000, safe integral integer bounds, scalar `enum` and `const` values matching their declared type, `anyOf`, and direct `#/$defs/name` or `#/definitions/name` references with ASCII identifier names. LocalBase rejects string minimum lengths, formats, remote or deeper references, other dialects, and keywords or combinations that the managed runtime would ignore.

For non-streaming responses, LocalBase validates ordinary assistant content when `finish_reason` is `stop`. A mismatch returns HTTP 502 with code `structured_output_validation_failed`. Refusals, tool calls, and truncated choices retain their original response semantics. Streaming remains incremental and relies on the runtime grammar rather than whole-response buffering.

See [Operating LocalBase](docs/operations.md) for queue limits, model-switch behavior, cancellation, telemetry, metadata, and compiled runtime qualification.

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

Stored API keys default to `inference:chat`, `inference:embeddings`, `inference:image`, `inference:video`, `inference:speech`, `inference:transcription`, and `models:read`. Existing keys migrate to this same set without administrative permissions. The `LOCALBASE_API_KEY` environment credential retains full access.

```bash
local-base keys create --name chat-client --scopes inference:chat,models:read --json
local-base keys list --json
local-base keys scopes key_ID --scopes inference:video,models:read --json
local-base keys rotate key_ID --json
local-base keys revoke key_ID --json
```

`keys scopes` replaces the complete scope set and takes effect on the next request without a restart. Use `--scopes ""` to grant no permissions. Scope lists accept only these permissions:

```text
inference:chat, inference:embeddings, inference:image, inference:video,
inference:speech, inference:transcription, models:read, models:manage,
configuration:read, configuration:manage, keys:read, keys:manage,
access:read, access:manage, sessions:read, sessions:revoke,
system:read, system:manage
```

Duplicates are removed and scopes are returned in the order above. Invalid scopes fail before any database changes. Creation and rotation show the secret once; list and scope output contain only key metadata. Rotation preserves scopes, expiry, revocation state, and the key ID used for video ownership. Scope changes also preserve revoked status. A missing, expired, revoked, or invalid credential returns HTTP 401; an active key without the required permission returns HTTP 403. Local CLI key management uses access to the data directory and does not require an API key.

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
