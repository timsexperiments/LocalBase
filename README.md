# LocalBase

LocalBase runs AI models on your own computer and serves them through one OpenAI-compatible API, with a web interface on top. It downloads models and their runtimes for you, and checks each model fits in memory before loading it.

It's one Go binary with no other dependencies to install. Your prompts and outputs stay on your machine, and there is no telemetry by default.

**Who it's for:** people who want local chat, embeddings, speech, images and video behind a single endpoint, on hardware they already own, and developers who want to point existing OpenAI clients at that hardware.

> **Status: pre-1.0.** The Go version described here is 0.2.0, and it **hasn't been released yet**. Until it is, the install script below installs the older TypeScript 0.1.0, so [build from source](#build-from-source) to try it. See [what is and isn't verified](#platforms-and-hardware).

<!-- Screenshots to add: Chat; Models library; Get models; Running now; Studio landing; Image, Speech, Transcribe, Video and Decisions studios; Settings; Admin; Chat on a phone. -->

## Quickstart

```sh
# 1. Install. The script verifies checksums, never uses sudo and installs to ~/.local/bin.
curl -fsSLO https://github.com/timsexperiments/LocalBase/releases/latest/download/install.sh
less install.sh
sh install.sh

# 2. Start the gateway as a user service on http://127.0.0.1:2273.
local-base start

# 3. Download a small model (about 3 GB) and create an API key. Copy the secret it prints.
local-base models install qwen3.5-4b-q4_k_m
local-base keys create --name quickstart
export API_KEY=<the secret>

# First chat.
curl http://127.0.0.1:2273/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.5-4b-q4_k_m","messages":[{"role":"user","content":"Say hello."}]}'
```

Requests must name a model. `local-base models recommend` lists what fits your machine. The first request also downloads the model's runtime, so it takes a little longer. Models that reason before answering can use a small `max_tokens` entirely on reasoning and return empty content, so leave room.

**The web interface needs one more step.** A fresh install has no browser sign-in, so `/app/login` refuses access until you [configure a sign-in provider](docs/authentication.md). There's no local-only sign-in yet, and API keys don't work in the browser.

### Build from source

Needs the Go version in [`go/go.mod`](go/go.mod).

```sh
git clone https://github.com/timsexperiments/LocalBase.git
cd LocalBase/go
go build -o local-base ./cmd/localbase
./local-base --version
```

## Platforms and hardware

| Platform | LocalBase runs | Runtimes it can download | Status |
| --- | --- | --- | --- |
| macOS, Apple Silicon | yes | all | **First-class.** The development and daily-use platform |
| Linux x86-64 | yes | all except CrispASR; the chat runtime is the Vulkan build | CI runs the tests and a fake backend. **Not qualified as an end-user install** |
| macOS Intel | yes | ffmpeg only | CLI only: bring your own backend executables |
| Linux ARM64 | yes | ffmpeg only | CLI only: bring your own backend executables |
| Windows | no | | Not planned |

**Not verified yet:**

- **NVIDIA on Linux.** Placement and GPU admission exist, tested with synthetic fixtures only. No real NVIDIA hardware has run them, and CUDA runtimes aren't managed for you. A custom CUDA backend can be registered with `local-base backends`.
- **Linux installs.** The service flow last passed in a disposable container, not on a real host.
- **Real phones and microphones.** Phone layouts and recording were checked in simulators and test browsers only.
- **Video through the web page.** The video API has generated real clips on a Mac, but the page has never had enough free memory for a real run.

**Memory** decides what you can run: about 5 GB free for the smallest chat model, and 20 GB or more for the larger ones. On Apple Silicon, memory is shared between CPU and GPU. `local-base doctor` reports what LocalBase sees, and `local-base models catalog` shows what fits.

## Features

- **OpenAI-compatible API.** Chat completions, embeddings, audio transcriptions, translations and speech, image generation and edits, and video jobs. See the [API overview](docs/api.md).
- **Studio tools.** Pages for Image, Speech, Transcribe, Video and Decisions, plus Chat, in the web interface.
- **Decisions.** `POST /v1/systemone` answers structured questions about a state with classifications, scores and yes/no probabilities, using decision models.
- **Memory-safe admission.** Before loading a model, LocalBase checks it fits and refuses clearly if it doesn't. Under memory pressure it evicts idle runtimes rather than letting the machine swap, and under critical pressure it stops active ones too.
- **Detached runs.** Generation from the web interface carries on when you switch tabs or lock your phone, and the page picks it back up when you return. API clients keep normal behaviour: disconnecting cancels.
- **Placement.** Each model can run automatically, GPU only, CPU only or with custom layer settings. This applies on machines with a discrete GPU; on Apple Silicon, memory is shared, so it doesn't.
- **Model management.** A curated catalog, plus imports from Hugging Face, direct URLs and local files, with licenses shown. LocalBase doesn't redistribute model weights. One model per type runs at a time.
- **Operations.** Structured logs, a redacted diagnostics bundle, declarative TOML configuration, and optional OTLP export.

## Security model

The gateway listens on loopback (`127.0.0.1`) by default. To reach it from elsewhere, publish it through an authenticated HTTPS proxy or tunnel.

| Who | How they authenticate | What they can do |
| --- | --- | --- |
| API clients | A scoped API key from `local-base keys` | Only the scopes on that key. By default, inference and reading the model list |
| Browser users | OpenID Connect (such as Google), Cloudflare Access or an emailed magic link, set up with `local-base access` | The permissions from their provider or role. By default, chat and reading the model list |
| Local administrator | Read and write access to the data directory | Everything, through the CLI. This is also the recovery path if you lock yourself out |

Browser sessions are opaque, HTTP-only cookies, and mutations carry a per-session CSRF token. See [authentication](docs/authentication.md) and [SECURITY.md](SECURITY.md).

## Documentation

- [Install, service setup, uninstall](docs/install.md) and [upgrade and rollback](docs/upgrade.md)
- [Configuration](docs/configuration.md), [declarative config](docs/declarative-config.md) and [operations](docs/operations.md)
- [API overview](docs/api.md) and [API details](docs/api-details.md)
- [Troubleshooting](docs/troubleshooting.md) and [FAQ](docs/faq.md)
- [Authentication](docs/authentication.md) and [user catalog entries](docs/user-catalog.md)
- [Privacy](docs/privacy.md), [model licenses](docs/model-licenses.md), [speech marking](docs/speech-marking.md), [third-party notices](THIRD_PARTY_NOTICES.md) and [name and marks](TRADEMARKS.md)
- [Design notes](go/DESIGN.md) and [changelog](CHANGELOG.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). New work goes in [`go/`](go/); [`src/`](src/README.md) is the legacy TypeScript version. To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

Copyright (C) 2026 TimsExperiments. LocalBase is licensed under [AGPL-3.0-or-later](LICENSE).

FFmpeg, which LocalBase downloads on demand for video conversion, is GPL-licensed. It isn't included in LocalBase and runs as a separate program; see [third-party notices](THIRD_PARTY_NOTICES.md).
