# local-base (Bun + TypeScript)

`local-base` is a local-network AI installer/manager CLI for coding-focused setups, with interactive configuration for LLM + STT model hosting.

## What to expect (current maturity)

- This is an **operator-friendly CLI bootstrapper** for local model hosting.
- It can configure defaults, recommend models, download model artifacts, and launch local servers.
- It is suitable for **single-machine Linux setups** where you are comfortable with CLI tools and managing model/runtime dependencies.
- It does **not** yet provide a built-in authenticated API gateway; API keys are managed by `local-base` so you can use them with your own proxy/client workflow.

## Interactive install experience

Running `local-base` with no subcommand starts setup.

You are guided through:

1. Root storage path (`root`)
2. LLM host/port/context size
3. STT host/port
4. Startup on boot toggle
5. Selected LLM models
6. Active/default LLM model
7. Selected STT models
8. Active/default STT model
9. Initial API key creation (if none exist)

Each prompt shows defaults and now uses a professional interactive terminal UI (single-select, multi-select, confirm Y/N, numeric input, text input). If you pass flags or use a TOML file, matching prompts are skipped.

## What it can configure

- **Model selection** across LLM, image, video, STT, TTS, and audio models
- **Storage location** (`--root`)
- **Runtime network settings** (`--host`, `--port`, `--stt-host`, `--stt-port`)
- **Context size** (`--ctx-size`)
- **Startup behavior** (`--startup-on-boot true|false`)
- **Default/active models** (`--active-llm`, `--active-stt`)
- **Default profile vs full configuration** (`--defaults` vs `--all` interactive)
- **Automatic first API key creation** (`--create-key true|false`)

## Project structure

All executable code is under `src/`:

- `src/cli.ts` — thin command dispatcher
- `src/domains/config/` — configuration domain (configure/init command handlers + workflow logic)
- `src/domains/auth/` — API key/auth domain (key commands + auth-related logic)
- `src/domains/models/` — model discovery/install domain (catalog/recommend/install/installed handlers)
- `src/domains/runtime/` — serving domain (`serve`, runtime orchestration)
- `src/domains/system/` — diagnostics domain (`doctor`)
- `src/domains/maintenance/` — destructive lifecycle domain (`reset`, `uninstall`)
- `src/domains/app/` — app-level shell/help command
- `src/utils/` — shared parsing/prompt/config helpers (`@inquirer/prompts` based interactive inputs)
- `src/catalog.ts` — shared model catalog metadata
- `src/manager.ts` — shared persistence + runtime integration (SQLite via Drizzle)
- `src/system.ts` — shared host spec detection
- `dist/local-base` — compiled Bun binary artifact (`bun run build`)

## Quick start

```bash
bun install
bun run build

# guided setup (default command)
./dist/local-base

# configure explicitly and force all prompts
./dist/local-base configure --all

# apply defaults non-interactively
./dist/local-base --defaults --host 0.0.0.0 --port 8000

# load options from TOML (flags still win)
./dist/local-base --config ./local-base.toml

# model catalogs
./dist/local-base catalog --kind llm
./dist/local-base catalog --kind image
./dist/local-base catalog --kind video
./dist/local-base catalog --kind stt

# recommendations
./dist/local-base recommend --kind llm --vram 12

# install
./dist/local-base install qwen2.5-coder-7b-instruct-q4_k_m

# start services
./dist/local-base serve
./dist/local-base serve --port 8787


# reset managed database + bootstrap defaults again
./dist/local-base reset --yes

# uninstall and remove everything local-base manages
./dist/local-base uninstall --yes
```


## How `serve` works (through our code)

Yes, `serve` is now the unified entrypoint and comes through our CLI code first.

- `local-base serve` routes to the unified wrapper implementation.
- It starts local LLM/STT runtimes when enabled and available, and proxies optional TTS/Image/Video upstreams.
- local-base is the control plane/orchestrator; inference is executed by the runtime servers.

## Unified API for LLM + STT (single endpoint)

Yes — this is supported by `serve`.

- Command: `local-base serve --port 8787`
- Wrapper routes:
  - LLM OpenAI-style traffic to llama upstream (`/v1/*`, except audio routes)
  - STT traffic from `/v1/audio/transcriptions` and `/v1/audio/translations` to whisper upstream
  - optional TTS route `/v1/audio/speech` when `--tts-upstream` is set
  - optional Image routes `/v1/images/*` when `--image-upstream` is set
  - optional Video routes `/v1/video/*` when `--video-upstream` is set
- Health check: `GET /health`
- API key auth: enabled by default. Supports `Authorization: Bearer <key>` and `x-api-key: <key>` (configurable with `--auth-mode`). Disable only for trusted local testing via `--auth false`

Notes:
- This is a lightweight proxy/orchestrator, not a full API gateway.
- Model inference still happens inside `llama-server` and `whisper-server`.
- It works well for local dev/single-node setups where one base URL for both LLM + STT is useful.





## Setting up TTS, Image, and Video infrastructure for unified proxy

`serve` can proxy these modalities, but you must run compatible upstream servers first.

### 1) Start upstream services

At minimum you need:

- **LLM upstream** (`llama-server`) and **STT upstream** (`whisper-server`) — these are started by `serve` itself when the modality is enabled.
- Optional dedicated upstreams you host separately:
  - **TTS upstream** (must accept your target request shape for `/v1/audio/speech`-style requests)
  - **Image upstream** (should expose image generation endpoints you want to map under `/v1/images/*`)
  - **Video upstream** (should expose video generation endpoints you want to map under `/v1/video/*`)

### 2) Bind each upstream to a local URL

Example local bindings:

- TTS upstream: `http://127.0.0.1:9001`
- Image upstream: `http://127.0.0.1:9002`
- Video upstream: `http://127.0.0.1:9003`

### 3) Start unified server with upstream mappings

```bash
./dist/local-base serve \
  --port 8787 \
  --auth-mode either \
  --tts-upstream http://127.0.0.1:9001 \
  --image-upstream http://127.0.0.1:9002 \
  --video-upstream http://127.0.0.1:9003
```

### 4) Verify routing and health

```bash
# health includes configured upstream metadata
curl http://127.0.0.1:8787/health

# LLM (proxied to llama upstream)
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<model>","messages":[{"role":"user","content":"hello"}]}' 

# STT (proxied to whisper upstream path)
curl -X POST http://127.0.0.1:8787/v1/audio/transcriptions \
  -H 'x-api-key: <API_KEY>' \
  -F file=@audio.wav -F model=whisper

# TTS (proxied only if --tts-upstream is configured)
curl -X POST http://127.0.0.1:8787/v1/audio/speech \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"tts","input":"hello"}' 

# Image (proxied only if --image-upstream is configured)
curl -X POST http://127.0.0.1:8787/v1/images/generations \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"a futuristic city"}' 
```

If a modality upstream is missing, unified proxy returns `501` with a hint to set the corresponding `--*-upstream` flag.

### Multimodal LLM clarification (image + text)

You are not misunderstanding: some LLM families are multimodal and can handle image+text in one model (for example vision-capable models).

- In this CLI, those requests still go through the LLM channel (`/v1/chat/completions`) when the underlying LLM runtime/model supports vision.
- Dedicated generation domains (TTS/Image/Video) are usually separate model servers, so unified API support proxies those to optional dedicated upstreams.

## Full command reference (commands, arguments, and options)

### 1) Default command / configure flow

**Command forms**

- `local-base`
- `local-base configure`

**Options**

- `--defaults` : Apply defaults and skip prompts unless required by missing values.
- `--all` : Force the full interactive prompt flow.
- `--config <file.toml>` : Load values from TOML; CLI flags override TOML.
- `--root <path>` : Root folder for DB, models, and managed files.
- `--host <host>` : LLM server host.
- `--port <n>` : LLM server port.
- `--ctx-size <n>` : LLM context size.
- `--stt-host <host>` : STT server host.
- `--stt-port <n>` : STT server port.
- `--startup-on-boot <true|false>` : Startup preference.
- `--llm-models <id1,id2>` : Comma-separated selected LLM model IDs.
- `--stt-models <id1,id2>` : Comma-separated selected STT model IDs.
- `--active-llm <id>` : Active/default LLM model ID.
- `--active-stt <id>` : Active/default STT model ID.
- `--create-key <true|false>` : Whether to create initial API key if no active keys exist.

### 2) Initialization

- `local-base init [--root <path>]`

Creates default configuration and managed directories/DB.

### 3) System diagnostics

- `local-base doctor [--json]`

Options:
- `--json` : print host specs as JSON.

### 4) Model catalog

- `local-base catalog [--kind llm|stt|tts|image|video|audio]`

Options:
- `--kind <kind>` : filter by model kind.

### 5) Model recommendations

- `local-base recommend [--kind llm|stt|tts|image|video|audio] [--vram <gb>]`

Options:
- `--kind <kind>` : recommendation category.
- `--vram <gb>` : target VRAM for compatibility filtering.

### 6) Installed model listing

- `local-base installed [--kind llm|stt|tts|image|video|audio]`

Options:
- `--kind <kind>` : list installed files for only that category.

### 7) Model install

- `local-base install <model_id>`

Arguments:
- `<model_id>` : catalog model ID.

### 8) Unified serving (single `serve` command)

- `local-base serve [--host <host>] [--port <port>] [--llm <true|false>] [--stt <true|false>] [--tts <true|false>] [--image <true|false>] [--video <true|false>] [--llm-host <host>] [--llm-port <port>] [--stt-host <host>] [--stt-port <port>] [--ctx-size <tokens>] [--stt-path <path>] [--llm-model-file <file>] [--stt-model-file <file>] [--auth <true|false>] [--auth-mode <bearer|x-api-key|either>] [--tts-upstream <url>] [--image-upstream <url>] [--video-upstream <url>]`

Options:
- `--host <host>` : wrapper API host (default `0.0.0.0`)
- `--port <port>` : wrapper API port (default `8787`)
- `--llm-host <host>` / `--llm-port <port>` : llama upstream bind
- `--stt-host <host>` / `--stt-port <port>` : whisper upstream bind
- `--ctx-size <tokens>` : llama context size
- `--stt-path <path>` : upstream whisper request path (default `/inference`)
- `--llm-model-file <file>` : explicit llm model file under `models/llm`
- `--stt-model-file <file>` : explicit stt model file under `models/stt`
- `--llm <true|false>` : enable/disable LLM routes/runtime (default auto: enabled if local LLM model file exists)
- `--stt <true|false>` : enable/disable STT routes/runtime (default auto: enabled if local STT model file exists)
- `--tts <true|false>` : enable/disable TTS proxy routes (default auto from `--tts-upstream`)
- `--image <true|false>` : enable/disable image proxy routes (default auto from `--image-upstream`)
- `--video <true|false>` : enable/disable video proxy routes (default auto from `--video-upstream`)
- `--auth <true|false>` : require API key auth on wrapper routes (default `true`)
- `--auth-mode <bearer|x-api-key|either>` : accepted credential style (default `either`)
- `--tts-upstream <url>` : upstream base URL for TTS routes (`/v1/audio/speech`)
- `--image-upstream <url>` : upstream base URL for image routes (`/v1/images/*`)
- `--video-upstream <url>` : upstream base URL for video routes (`/v1/video/*`)

Behavior:
- auto-disables routes when required model files/upstreams are not configured
- starts local runtime processes only for enabled local modalities (LLM/STT)
- exposes one HTTP API for all enabled task types
- proxies `/v1/audio/transcriptions` to STT upstream and routes other `/v1/*` traffic to LLM upstream
- optionally proxies TTS/image/video routes when upstream URLs are configured

### 9) API key management

- `local-base keys list`
- `local-base keys create [--name <label>] [--expires-days <n>]`
- `local-base keys rotate <key_id>`
- `local-base keys revoke <key_id>`

Arguments:
- `<key_id>` : key identifier from `keys list`.

Options:
- `--name <label>` : human label when creating.
- `--expires-days <n>` : optional expiry in days.

### 10) Database reset / reinstall bootstrap

- `local-base reset [--root <path>] [--yes|-y]`

Options:
- `--root <path>` : explicit managed root.
- `--yes` / `-y` : required confirmation for destructive reset.

Behavior:
- Deletes `<root>/local-base.db`
- Recreates default config in fresh DB

### 11) Full uninstall of managed data

- `local-base uninstall [--root <path>] [--yes|-y]`

Options:
- `--root <path>` : explicit managed root.
- `--yes` / `-y` : required confirmation for destructive removal.

Behavior:
- Deletes the entire managed root directory recursively.

## Command-by-command user flow (what you’ll be asked / see)

### `local-base` or `local-base configure`

You will see prompts (unless values are already supplied by flags/TOML):

1. `Root directory [<default>]`
2. `LLM host [<default>]`
3. `LLM port [<default>]`
4. `LLM context size [<default>]`
5. `STT host [<default>]`
6. `STT port [<default>]`
7. `Start services on boot [true|false]`
8. list of recommended LLMs printed with storage/VRAM/coding hints
9. `Selected LLM model ids (comma-separated) [<default>]`
10. `Active LLM model id [<default>]`
11. list of recommended STT models printed with storage/VRAM hints
12. `Selected STT model ids (comma-separated) [<default>]`
13. `Active STT model id [<default>]`
14. If no active key exists: `No API keys found. Create one now [true]`

Then CLI prints:
- DB save path (`<root>/local-base.db`)
- chosen LLM/STT selections
- initial key (id/prefix/secret once) if created

### `local-base doctor`

You’ll see OS, CPU, RAM, GPU model, GPU VRAM, and a readiness status line.

### `local-base catalog`

You’ll get one line per model with kind, ID, size, min VRAM, storage, status, plus modalities/features and licensing catch notes.

### `local-base recommend`

You’ll get top matching models for chosen kind and VRAM.

### `local-base install <model_id>`

The CLI downloads the model artifact into managed model directories and prints the installed path.

### `local-base serve`

CLI starts one wrapper API and enables modalities automatically based on available local models/upstreams (unless overridden with flags).

- `/v1/chat/completions` and similar LLM routes are proxied to llama-server when LLM is enabled.
- `/v1/audio/transcriptions` and `/v1/audio/translations` are proxied to whisper-server when STT is enabled.

- `/v1/chat/completions` and similar LLM routes are proxied to llama-server.
- `/v1/audio/transcriptions` and `/v1/audio/translations` are proxied to whisper-server.
- `/health` reports wrapper/upstream metadata.

### `local-base keys ...`

- `list`: prints key IDs + metadata.
- `create`: prints new key ID + secret (secret shown once).
- `rotate`: prints new secret for existing key (once).
- `revoke`: marks key revoked and prints confirmation.

### `local-base reset --yes`

CLI confirms reset completion and indicates defaults were re-bootstrapped.

### `local-base uninstall --yes`

CLI confirms all managed data under root was removed.

## API key management

```bash
# list keys
./dist/local-base keys list

# create a key
./dist/local-base keys create --name dev-laptop --expires-days 90

# rotate a key (prints new secret once)
./dist/local-base keys rotate <key_id>

# revoke a key
./dist/local-base keys revoke <key_id>
```

## Connecting clients

When `serve` starts, the CLI prints:

- OpenAI-compatible base URL (`http://<host>:<port>/v1`)
- `curl` examples for `/v1/models` and `/v1/chat/completions`
- reminder to use keys from `local-base keys create` in your client/proxy

This endpoint can be used by OpenAI-compatible coding tools (Continue/Cline/OpenCode-style clients) by setting base URL + model name.



### OpenCode/OpenAI-compatible setup note

For maximum compatibility with OpenAI-compatible tools (including OpenCode-style clients), keep `--auth-mode either` (default).
For vision-capable LLMs, send multimodal chat payloads to `/v1/chat/completions` (model/runtime dependent).
That allows either:
- `Authorization: Bearer <API_KEY>` (OpenAI-style default)
- `x-api-key: <API_KEY>` (common alternate API key style)

## OpenCode / bring-your-own-endpoint compatibility

Yes — this is intended to be compatible with tools that let you bring your own OpenAI-style endpoint.

- For LLM clients, point base URL to `http://<host>:<port>/v1` and set API key.
- With unified serving, default auth mode `either` supports both:
  - `Authorization: Bearer <API_KEY>`
  - `x-api-key: <API_KEY>`
- This aligns with typical OpenAI-compatible configuration flows in OpenCode-like clients.



## OpenAI/OpenCode compatibility (what is standard vs passthrough)

Short answer: **mostly yes for standard OpenAI-style LLM calls**, because the unified gateway proxies to runtimes that expose OpenAI-compatible APIs.

### What should work out of the box

When using `serve`, OpenCode/OpenAI-style clients should generally work with:

- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/completions` (if supported by selected upstream/runtime)
- `POST /v1/audio/transcriptions` (STT route, proxied to whisper upstream path)

### Important caveat

The gateway is primarily a **proxy/orchestrator**. Request/response schema fidelity is therefore upstream-dependent:

- LLM schema behavior comes from `llama-server` (or your chosen LLM upstream).
- STT schema behavior comes from `whisper-server` (or your chosen STT upstream).
- Optional TTS/Image/Video behavior comes from the configured upstreams for those modalities.

If an upstream deviates from OpenAI schema details, the gateway will pass through that behavior.

### Practical OpenCode setup

Use:

- Base URL: `http://<local-base-host>:<local-base-port>/v1`
- API key: a key created by `local-base keys create`
- Auth mode recommendation: keep `--auth-mode either` for maximum client compatibility.

## Runtime prerequisites

- **Target platform:** Linux (Ubuntu LTS recommended for current release flow)
- Data is persisted in SQLite (`<root>/local-base.db`) using Drizzle ORM
- `bun` for CLI runtime/build
- `curl` for downloads
- `nvidia-smi` for GPU detection (optional but recommended)
- `llama-server` from `llama.cpp` for LLM serving
- `whisper-server` from `whisper.cpp` (or compatible) for STT serving

## CI/CD

- CI runs on pushes/PRs to `main` and validates:
  - install (`bun install --frozen-lockfile`)
  - typecheck (`bun run typecheck`)
  - smoke check (`bun run check`)
- Releases are published to GitHub Releases automatically when a tag matching `v*` is pushed.
- Current release packaging publishes a Linux binary artifact (`local-base-linux-x64`).

## Example TOML config

```toml
root = "/data/local-base"
host = "0.0.0.0"
port = 8000
ctxSize = 8192
sttHost = "0.0.0.0"
sttPort = 8080
startupOnBoot = false
selectedLlmModels = ["qwen2.5-coder-7b-instruct-q4_k_m"]
selectedSttModels = ["whisper-base-q8_0"]
activeLlmModel = "qwen2.5-coder-7b-instruct-q4_k_m"
activeSttModel = "whisper-base-q8_0"
```
