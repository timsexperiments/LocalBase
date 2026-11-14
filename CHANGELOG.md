# Changelog

Notable changes to LocalBase are documented here. This project follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and uses [Semantic Versioning](https://semver.org/).

See the [versioning policy](docs/versioning.md) for what is stable before 1.0.

## [Unreleased]

## [0.2.0] - Unreleased

LocalBase is rewritten in Go. It's one static binary with the UI built in, and it takes over an existing 0.1 data directory in place. Rolling back to 0.1.0 is a binary swap; see [upgrade and rollback](docs/upgrade.md).

### Added

- **First-run sign-in link:** `local-base access login-link`, for a browser on the same computer only. See [first-run sign-in](docs/first-run-sign-in.md).
- **Failed installs say why:** `error.reason`. See [install failures](docs/install-failures.md).
- **Host and admission API:** `GET /_localbase/host` and `GET /_localbase/models/{id}/admission`.
### Added

- **Studio pages:** Image, Speech, Transcribe, Video and Decisions, in a redesigned web interface with Chat, Models, Running now, voices, Settings and Admin.
- **Detached runs:** generation in the web interface carries on when you leave the page, and the page re-attaches when you return. Checkpoints in `<root>/runs/` hold model output in plaintext for up to 30 minutes after a run ends.
- **Placement:** run each model automatically, GPU only, CPU only or with custom layers. NVIDIA support is tested with synthetic fixtures only.
- **Model imports:** from Hugging Face, direct URLs and local files, with private download headers, automatic backend detection and a per-device override.
- **Fit-aware default chat model:** an ordered list of defaults, from which LocalBase picks the first that fits.
- **Explicit unload:** `POST /_localbase/models/{id}/unload`.
- **Admin lockout protection** in the API: `409 would_lock_out_admins`.
- **Speech:** CrispASR on macOS ARM64, with `local-base speech marking on|off`. See [speech marking](docs/speech-marking.md).
- **Image API:** `data[i].seed` echoes each image's seed. Long speech that gets cut is flagged by `X-Localbase-Audio-Truncated`.
- **Release tooling:** checksums, an SBOM, provenance attestation and a checksum-verifying installer that keeps the previous binary.

### Changed

These differ from 0.1.0 for API clients:

- `serve --no-auth` refuses a non-loopback host, and answers 403 to foreign `Host` values, proxy headers and foreign origins.
- The database is created 0600, and `serve` tightens older data directories.
- An unparseable multipart body returns 400 `validation_failed`, not 500 `gateway_error`.
- The wrong method on a JSON inference route returns 405 with `Allow`, not 400.
- A chat request with an empty `messages` list is refused.
- Only a chat model becomes the active chat model; a decision or embedding model no longer does.
- A request after a permanent model start failure, a request cut off by a memory emergency and an input over the backend's batch size each have revised responses. See `go/DESIGN.md` §13.6.

### Removed

- New GitHub sign-in registrations. Use OIDC, Cloudflare Access or magic links. Existing GitHub registrations can only be listed and removed.

### Fixed

- **Diagnostics bundle on Linux and Intel Macs:** each model's memory verdict and `systemMemoryGb` now use this computer's memory, as `models list` and `models show` do. They were `0` and "won't fit" for every model.

### Known limits

- Not qualified on real NVIDIA hardware, and Linux isn't qualified as an end-user install.
- Not checked on real phones or with a real microphone, and video hasn't had a real run through the web page.
- Windows isn't supported.

## [0.1.0] - 2026-10-12

### Added

- Bun CLI and web interface for an OpenAI-compatible local AI gateway.
- Managed local runtimes for chat, embeddings, speech, image generation, and video, with model management and process supervision.
- SQLite-backed configuration, API keys, and gateway access controls.
- OpenAI-compatible image edits and experimental TI2V image-to-video inputs.
- Experimental, opt-in Wan S2V speech-to-video catalog profile.
- Image edits structurally validate PNG and JPEG inputs, bound PNG decompression, and limit dimensions to 2048 pixels per side and 4.2 megapixels. WebP inputs are not accepted because the pinned backend decoder does not support them. Output sizes are limited to 256, 512, or 1024 square.
- Open source governance, security, and contribution documentation.
