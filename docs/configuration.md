# Configuration

Settings live in the SQLite database in the data directory. Change them with:

- `local-base configure`, for individual settings;
- `local-base config apply --file config.toml`, for [declarative configuration](declarative-config.md);
- the Settings page in the web interface.

Run `local-base <command> --help` for every option, and `local-base doctor` to see what is in effect.

## Data directory

The root is chosen from `--root`, then `LOCALBASE_ROOT`, then the default `~/.local/share/local-base`.

| Path under the root | Contents |
| --- | --- |
| `local-base.db` | Configuration, API keys, model records |
| `models/` | Model files |
| `bin/runtimes/` | Downloaded runtimes, pinned by checksum |
| `catalog.d/` | Your own catalog entries |
| `voices/` | Custom voice references |
| `logs/` | Redacted JSON Lines logs |
| `runs/` | Short-lived checkpoints of detached runs, including model output in plaintext |
| `ui-access.json` | Browser sign-in configuration, including provider secrets (0600) |

## Default ports

| Service | Default | Option |
| --- | --- | --- |
| Gateway | `2273` | `--port`, `LOCALBASE_PORT` |
| Chat runtime | `18000` | `--llm-port` |
| Speech-to-text runtime | `18080` | `--stt-port`, `LOCALBASE_STT_PORT` |
| Image runtime | `8090` | `--image-port` |
| Video runtime | `8091` | `--video-port` |
| Text-to-speech runtime | `8092` | `--tts-port` |

Runtime ports stay on loopback unless you change their hosts.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `LOCALBASE_ROOT` | Data directory |
| `LOCALBASE_HOST`, `LOCALBASE_PORT` | Gateway address |
| `LOCALBASE_STT_HOST`, `LOCALBASE_STT_PORT` | Speech-to-text runtime address |
| `LOCALBASE_CTX_SIZE` | Chat context limit |
| `LOCALBASE_API_KEY` | **A full-access key the gateway accepts**, read when it starts. Leave it unset unless you mean that; use `local-base keys` for scoped keys |
| `LOCALBASE_DEFAULT_MAX_TOKENS` | Default generation limit |
| `LOCALBASE_CATALOG_FEED_URL` | Remote catalog feed |
| `LOCALBASE_DOWNLOAD_CA_FILE` | Extra CA bundle for downloads |
| `LOCALBASE_GPU_WORKING_SET_GB` | Override the detected GPU working-set limit |
| `LOCALBASE_LLAMA_SERVER`, `LOCALBASE_WHISPER_SERVER`, `LOCALBASE_SD_SERVER`, `LOCALBASE_TTS_SERVER`, `LOCALBASE_CRISPASR`, `LOCALBASE_FFMPEG` | Use your own backend executable |
| `HF_TOKEN`, `HF_ENDPOINT` | Hugging Face access token and endpoint |
| `OTEL_EXPORTER_OTLP_*`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Standard OpenTelemetry settings, which override saved ones |

`LOCALBASE_SERVICE_ID`, `LOCALBASE_SERVICE_TOKEN` and `LOCALBASE_DEVICE_ID` are internal, so don't set them unless a document tells you to.
