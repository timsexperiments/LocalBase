# LocalBase

`local-base` is a lightweight, zero-external-dependency local AI serving wrapper and developer proxy control plane written in Bun and TypeScript. It orchestrates, installs, and supervises local LLM (`llama-server`) and STT (`whisper-server`) runtimes, exposing a single, unified, OpenAI-compatible API gateway.

---

## Core Architectural Features

*   **On-Demand Lazy Loading**: Upstream backend servers (`llama-server` and `whisper-server`) are only booted on the first API request targeting that model, preserving VRAM when idle.
*   **Self-Healing Supervisor**: Monitors child processes and automatically recovers from unexpected crashes using doubling exponential backoff (1s to 16s) and a safety budget (max 5 crashes in 5 minutes).
*   **On-the-Fly Model Switching**: Intercepts `/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings` requests. If the requested model differs from the active one, the supervisor halts the current backend, updates configurations, synchronizes editor settings, and boots the new model before routing the request.
*   **Dynamic Context Sizing**: Automatically detects host hardware (VRAM/RAM) and calculates the optimal context ceiling for the active model: `min(recommendedCtxForHardware, maxConfigCtx)`.
*   **Automated Editor Syncing**: Synchronizes model choices, context limits, and endpoints in real-time with **OpenCode** (`opencode.jsonc`) and **Continue.dev** (`config.json`).
*   **Lazy-Load Shield (`/v1/models`)**: Intercepts model listing requests and serves catalog metadata instantly without triggering LLM process activation during IDE background polling.

---

## Quick Start

```bash
# 1. Install dependencies and compile the binary
bun install
bun run build       # Compiles standalone native executable to dist/local-base
bun run build:js    # Compiles NPM-executable JS to dist/cli.js

# 2. Run interactive guided configuration
./dist/local-base

# 3. Download and install a model from catalog
./dist/local-base catalog --kind llm
./dist/local-base install qwen2.5-coder-7b-instruct-q4_k_m

# 4. Start the unified serving gateway
./dist/local-base serve
```

*For a full description of all subcommands, arguments, and options, run:*
```bash
./dist/local-base --help
```

---

## Unified API Mappings

The wrapper binds to a single port (default `8787`) and exposes standard OpenAI endpoints:

*   **LLM (Chat & Completions)**: Proxies `/v1/chat/completions` and `/v1/completions` to `llama-server`. Maps modern OpenAI `"developer"` role inputs to `"system"` for tokenizer compatibility.
*   **Codebase Indexing**: Proxies `/v1/embeddings` to `llama-server`.
*   **Model List**: Intercepts `/v1/models` to serve active/selected configurations instantly.
*   **STT (Audio Transcriptions)**: Proxies `/v1/audio/transcriptions` and `/v1/audio/translations` to `whisper-server`.
*   **Modality Proxies**: Directs `/v1/audio/speech` (TTS) and `/v1/images/*` (Image generation) to optional external upstreams via `--tts-upstream` and `--image-upstream`.
*   **Diagnostics & Health**: Exposes `/health` to verify all upstream bindings and active statuses.

---

## IDE Integration Setup

### OpenCode & Continue.dev
Configuration is performed automatically. Upon running `./dist/local-base serve` or `./dist/local-base configure`, the wrapper resolves your active model, calculated context limits, and credentials, writing them directly to:
*   `~/.config/opencode/opencode.jsonc` (OpenCode)
*   `~/.continue/config.json` (Continue.dev)

### Cursor
To route Cursor queries through LocalBase:
1. Open Cursor Settings -> **Models**.
2. Under **OpenAI API Key**, add a key generated via `local-base keys create` (or your static `LOCALBASE_API_KEY`).
3. Under **Override URL**, set `http://localhost:8787/v1`.
4. Define your active model (e.g. `qwen2.5-coder-7b-instruct-q4_k_m`) under the Model list.

---

## Development Setup & Running

### Prerequisites
- [Bun](https://bun.sh) runtime (latest)
- SQLite3 (installed on host)

### Local Setup
1. Clone the repository and navigate to the directory:
   ```bash
   git clone git@github.com:timsexperiments/LocalBase.git
   cd LocalBase
   ```
2. Install dependencies:
   ```bash
   bun install --frozen-lockfile
   ```

### Running in Development
Execute the CLI directly from source using the Bun runtime:
```bash
bun run local-base [command] [options]
# Example:
bun run local-base doctor
```

### Build & Package Validation
*   **Typecheck**: `bun run typecheck`
*   **Build Standalone Binary**: `bun run build` (outputs to `dist/local-base`)
*   **Build JS NPM Entrypoint**: `bun run build:js` (outputs to `dist/cli.js`)
*   **Verify NPM Package Size & Contents**: `npm pack`

### CI/CD Pipelines
*   **CI Workflow**: Automatically validates lockfiles, executes TypeScript compilation checks, and runs CLI smoke tests on push/PR.
*   **Release Workflow**: Triggered on tag pushes (`v*`). Automatically cross-compiles native binaries for macOS (ARM64/x64), Linux (ARM64/x64), and Windows (x64), computes SHA-256 integrity checksums, publishes a GitHub Release, and publishes the JS CLI to NPM with secure provenance.

---

## Support & Contribution

*   **Bug Reports & Requests**: Submit an issue on [GitHub Issues](https://github.com/timsexperiments/LocalBase/issues).
*   **Contributing**: Create a branch (prefixed with `tim/` or your name), apply modifications, verify using `bun run typecheck`, and submit a Pull Request.
