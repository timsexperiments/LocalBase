# LocalBase

LocalBase runs AI models on your own computer and serves them through one OpenAI-compatible API, with a web interface on top. It downloads models and their runtimes for you, and checks each model fits in memory before loading it.

It's one Go binary with no other dependencies to install. Your prompts and outputs stay on your machine, and there is no telemetry by default.

**Who it's for:** people who want local chat, embeddings, speech, images and video behind a single endpoint, on hardware they already own, and developers who want to point existing OpenAI clients at that hardware.

> **Status: pre-1.0.** The Go version described here is 0.2.0, and it **hasn't been released yet**. Until it is, the install script below installs the older TypeScript 0.1.0, so [build from source](#build-from-source) to try it. See [what is and isn't verified](#platforms-and-hardware).

<!-- Screenshots to add: Chat; Models library; Get models; Running now; Studio landing; Image, Speech, Transcribe, Video and Decisions studios; Settings; Admin; Chat on a phone. -->

## Quickstart

**macOS on Apple Silicon is the tested path.** On Linux x86-64 without a GPU, chat worked in testing only with the memory reserve lowered, and slowly. A 16 GB machine is refused at the default reserve. See [what runs where](docs/platforms.md).

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

**The web interface:** `local-base start` prints a [one-time sign-in link](docs/first-run-sign-in.md), and `local-base access login-link` makes another. **The link only works from a browser on the same computer.** A phone or another computer needs an [identity provider](docs/authentication.md). API keys don't work in the browser.

### Build from source

Needs the Go version in [`go/go.mod`](go/go.mod).

```sh
git clone https://github.com/timsexperiments/LocalBase.git
cd LocalBase/go
go build -o local-base ./cmd/localbase
./local-base --version
```

## Platforms and hardware

See [what runs where](docs/platforms.md) for what has actually been run.

| Platform | LocalBase runs | Runtimes it can download | Status |
| --- | --- | --- | --- |
| macOS, Apple Silicon | yes | all | **First-class.** The development and daily-use platform |
| Linux x86-64 | yes | all except CrispASR; the chat runtime is the Vulkan build | **Without a GPU:** chat worked in testing only with a lowered memory reserve, at about 7 tokens a second. Kokoro doesn't run, and image and transcription need Vulkan. With a GPU, untested. The tests and installer pass in CI, with a fake backend. **Not qualified as an end-user install** |
| macOS Intel | yes | ffmpeg only | Chat, image and transcription are refused until you supply your own backend. That hasn't been tried |
| Linux ARM64 | yes | ffmpeg only | Chat, image and transcription are refused until you supply your own backend. That hasn't been tried |
| Windows | no | | Not planned |

**Not verified yet:**

- **NVIDIA on Linux.** Placement and GPU admission exist, tested with synthetic fixtures only. No real NVIDIA hardware has run them, and CUDA runtimes aren't managed for you. A custom CUDA backend can be registered with `local-base backends`.
- **Linux services and real inference.** The installer and `serve` pass in CI on Ubuntu, but `local-base start` has only run in a disposable container, and real models on Linux are unqualified.
- **Real phones and microphones.** Phone layouts and recording were checked in simulators and test browsers only.
- **Video through the web page.** The video API has generated real clips on a Mac, but the page has never had enough free memory for a real run.

**Memory** decides what you can run. The smallest chat model is planned at about 5 GB, and the larger ones at 20 GB or more. By default LocalBase also keeps at least 8 GiB free for the system on top of that, so even the smallest needs about 13 GiB free. A 16 GB machine may refuse it. On Apple Silicon, memory is shared between CPU and GPU. `local-base doctor` reports what LocalBase sees, and `local-base models catalog` shows what fits.

## Features

- **OpenAI-compatible API.** Chat completions, embeddings, audio transcriptions, translations and speech, image generation and edits, and video jobs. See [connect an app](docs/connect.md) and the [API overview](docs/api.md).
- **Studio tools.** Pages for Image, Speech, Transcribe, Video and Decisions, plus Chat, in the web interface.
- **Decisions.** `POST /v1/systemone` answers structured questions about a state with classifications, scores and yes/no probabilities, using decision models.
- **Memory-safe admission.** Before loading a model, LocalBase checks it fits, and refuses if it doesn't. Under memory pressure it evicts idle runtimes to avoid swapping, and under critical pressure it stops active ones too.
- **Detached runs.** Generation from the web interface carries on when you leave the page, and the page picks it back up when you return. This was checked in test browsers, not yet on a real phone. API clients keep normal behaviour: disconnecting cancels.
- **Placement.** Each model has a setting: automatic, GPU only, CPU only or custom layers. It doesn't apply on Apple Silicon, where memory is shared. On Linux it's unverified with a real GPU.
- **Model management.** A curated catalog, plus imports from Hugging Face, direct URLs and local files, with licenses shown. LocalBase doesn't redistribute model weights. One model per type runs at a time.
- **Operations.** Structured logs, a redacted diagnostics bundle, declarative TOML configuration, and optional OTLP export.

## Security model

The gateway listens on loopback (`127.0.0.1`) by default. To reach it from elsewhere, publish it through an authenticated HTTPS proxy or tunnel.

| Who | How they authenticate | What they can do |
| --- | --- | --- |
| API clients | A scoped API key from `local-base keys` | Only the scopes on that key. By default, inference and reading the model list |
| Browser users | OpenID Connect (such as Google), Cloudflare Access or an emailed magic link, set up with `local-base access` | The permissions from their provider or role. By default, chat and reading the model list |
| You, on this computer | A [one-time link](docs/first-run-sign-in.md) from `local-base access login-link` | Everything. It works only from a browser on the same computer |
| Local administrator | Read and write access to the data directory | Everything, through the CLI. This is also the recovery path if you lock yourself out |

Browser sessions are opaque, HTTP-only cookies, and mutations carry a per-session CSRF token. `--no-auth` is [for local development only](docs/no-auth.md) and must never be tunneled. See [authentication](docs/authentication.md) and [SECURITY.md](SECURITY.md).

## Documentation

- [Install, service setup, uninstall](docs/install.md) and [upgrade and rollback](docs/upgrade.md), and [the upgrade command](docs/upgrade-command.md), and [backup and restore](docs/backup.md)
- [Configuration](docs/configuration.md), [declarative config](docs/declarative-config.md) and [operations](docs/operations.md)
- [Connect an app](docs/connect.md)
- [Usage and activity](docs/usage.md)
- [API overview](docs/api.md), [API reference](docs/api-reference.md) and [API details](docs/api-details.md)
- [Troubleshooting](docs/troubleshooting.md) and [FAQ](docs/faq.md)
- [Authentication](docs/authentication.md), [first-run sign-in](docs/first-run-sign-in.md), [`--no-auth`](docs/no-auth.md) and [user catalog entries](docs/user-catalog.md)
- [Why a model was refused](docs/model-refusals.md), [the memory reserve](docs/memory-reserve-guide.md), [GPU memory limit](docs/gpu-memory-limit.md) and [install failures](docs/install-failures.md)
- [Privacy](docs/privacy.md), [where secrets live](docs/secrets.md), [model licenses](docs/model-licenses.md), [speech marking](docs/speech-marking.md), [third-party notices](THIRD_PARTY_NOTICES.md) and [name and marks](TRADEMARKS.md)
- [Design notes](go/DESIGN.md) and [changelog](CHANGELOG.md)
- [Versioning and deprecation](docs/versioning.md), [release checklist](docs/release-checklist.md) and [TypeScript retirement](docs/typescript-retirement.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The code is in [`go/`](go/); the TypeScript version is retired, and its 0.1.0 release stays the rollback. To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

Copyright (C) 2026 TimsExperiments. LocalBase is licensed under [AGPL-3.0-or-later](LICENSE).

FFmpeg, which LocalBase downloads on demand for video conversion, is GPL-licensed. It isn't included in LocalBase and runs as a separate program; see [third-party notices](THIRD_PARTY_NOTICES.md).
