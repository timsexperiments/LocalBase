# Changelog

Notable changes to LocalBase are documented here. This project follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and uses [Semantic Versioning](https://semver.org/).

Before 1.0, minor `0.x` releases may include breaking changes. No migration guarantees are made before 1.0; reset or reinstall may be required when upgrading.

## [Unreleased]

## [0.1.0] - 2026-10-12

### Added

- Bun CLI and web interface for an OpenAI-compatible local AI gateway.
- Managed local runtimes for chat, embeddings, speech, image generation, and video, with model management and process supervision.
- SQLite-backed configuration, API keys, and gateway access controls.
- OpenAI-compatible image edits and experimental TI2V image-to-video inputs.
- Experimental, opt-in Wan S2V speech-to-video catalog profile.
- Image edits structurally validate PNG and JPEG inputs, bound PNG decompression, and limit dimensions to 2048 pixels per side and 4.2 megapixels. WebP inputs are not accepted because the pinned backend decoder does not support them. Output sizes are limited to 256, 512, or 1024 square.
- Open source governance, security, and contribution documentation.
