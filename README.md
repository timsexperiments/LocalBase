# LocalBase

LocalBase is a Bun/TypeScript unified, OpenAI-compatible gateway for local AI runtimes. It listens on port `2273` by default and manages model processes behind one API surface.

## Current capabilities

- **LLM** chat and completions, including configured-model switching.
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

## Roadmap

TIM-21 adds configurable and hardware-aware llama-server request slots. Dynamic model pools, LRU eviction, text-to-speech, and video generation remain future work.

## Contributing

Keep documentation and behavior aligned, use Bun for project commands, and verify changes with the checks above before opening a pull request. Report bugs and requests through [GitHub Issues](https://github.com/timsexperiments/LocalBase/issues).
