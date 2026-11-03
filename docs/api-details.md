# API details

Request and response contracts carried over from the 0.1 documentation. The Go gateway keeps these HTTP contracts, checked by the contract suite in `go/contract/`, so read this for the detail behind [the API overview](api.md). Where this text mentions TypeScript internals or differs from the running gateway, the gateway wins, and a correction is welcome.

## Current capabilities

- **LLM** OpenAI-compatible chat completions, including configured-model switching.
- **Embeddings** for local indexing and search.
- **Decisions** through `POST /v1/systemone` for decision models such as Clef: classification, scoring, and yes/no probabilities in one forward pass.
- **STT** audio transcriptions and translations.
- **TTS** through `/v1/audio/speech` with the bounded Qwen3 TTS base model.
- **Image generation** through the OpenAI-compatible `/v1/images/generations` endpoint.
- **Image editing** through multipart `POST /v1/images/edits` for catalog image-to-image models.
- **Image-to-video** through `POST /v1/videos` with a PNG `input_reference` on TI2V profiles.
- Lazy preparation of LLM, STT, TTS, and image runtimes on first use.
- Self-healing process supervision with bounded restart backoff.
- Zod request and response validation.
- SQLite-backed configuration and API-key storage.
- Hardware-aware context sizing (`auto`) or an explicit persisted context ceiling (`configure --ctx-mode explicit`), with the effective limit and every reduction explained in `/v1/models`. `GET /v1/models/{model}` returns one entry of that list, under the same `models:read` scope and visibility rules; IDs a key cannot list return 404 `model_not_found`.

The runtime keeps one active model per service. Memory pressure can evict idle runtimes or stop all runtimes in an emergency. Simultaneous multi-model process pools are not supported.

### Model metadata

Authenticated `GET /_localbase/models` and `GET /_localbase/models/:modelId` return catalog identity, every declared artifact checksum and size, catalog memory estimates, and local selection, installation, and runtime state. `selected` reports configured selection. `runtime` reports the observed applied supervisor, so a replacement may be selected while the previous model drains. The gateway captures both before file inspection. Runtime metadata separates resolved native slots, active admissions, arithmetic availability, immediate dispatch eligibility, and bounded waiting-room capacity. Reads do not start runtimes or hash model files. Capability fields remain `null` without an authoritative value; the TTS model reports its WAV, catalog-voice, and cold-request contract. Each `/v1/models` entry also lists `capabilities` as `[{ "id", "source" }]`, where `source` is `declared` (catalog), `detected` (installed files), or `runtime` (the running engine). `local-base models show <id>` prints one model's provenance, files, capabilities with their sources, memory figures, and fit on this machine; add `--json` for the same data. In the browser, **Details** on a catalog card opens the same view at a linkable `?panel=catalog&details=<id>` URL.

TTS is disabled by default. The first supported model is `qwen3-tts-1.7b-base-q4_k_m`. Requests must set `voice` to `default`, `harbor`, `willow`, or the id of a stored custom voice (below); omitted `response_format` defaults to WAV, unlike OpenAI's MP3 default, so clients that omit the format receive WAV bytes. The only supported explicit format is `wav`. `default` uses no reference file. Harbor and Willow are fixed, checksum-pinned CC0 references from Kyutai's verified Unmute voice donations ([provenance](https://huggingface.co/kyutai/tts-voices/tree/323332d33f997de8394f24a193e1a76df720e01a/voice-donations)). Speech requests cannot supply audio paths, URLs, or uploads; only an administrator can add a reference, through the voice endpoints below. Speed is fixed at `1`, instructions are unsupported, and input is limited to 256 characters. Each request runs a cold, bounded native generation and returns PCM16 mono WAV at 24 kHz. Human voice quality has not been assessed.

### Custom voices

The Qwen3 TTS runtime clones a voice zero-shot from a short reference recording, so a custom voice is just a stored recording; nothing is trained. The runtime encodes the clip into a speaker embedding (ECAPA-TDNN x-vector) on each request and needs no transcript. LocalBase stores each reference as 24 kHz mono PCM16 WAV, the rate the speaker encoder resamples to, under `<root>/voices/<id>.wav` (directory `0700`, file `0600`), with its name, SHA-256, duration, creation time, creating principal and consent attestation in the database. Uploads must be 3 to 30 seconds of RIFF/WAVE PCM or float audio (8 to 192 kHz, up to 8 channels, at most 12 MiB). They are decoded and resampled in TypeScript, never by a native decoder, and the rewritten file drops every metadata chunk. Silent clips are rejected. The browser converts microphone recordings and other formats to WAV before upload. At most 32 voices are stored.

| Method and path | Permission | Purpose |
| --- | --- | --- |
| `GET /_localbase/voices` | `models:read` | List voices, limits and the consent statement; `createdBy` only for managers |
| `POST /_localbase/voices` | `models:manage` | Create from multipart `name`, `consent=true` and an `audio` file |
| `GET /_localbase/voices/{id}` | `models:read` | Read one voice |
| `PATCH /_localbase/voices/{id}` | `models:manage` | Rename with JSON `{"name": "..."}` |
| `DELETE /_localbase/voices/{id}` | `models:manage` | Delete the voice and its recording |
| `GET /_localbase/voices/{id}/audio` | `models:manage` | Play back the stored reference |

```bash
curl http://127.0.0.1:2273/_localbase/voices -H 'Authorization: Bearer <ADMIN_KEY>' \
  -F name=Narrator -F consent=true -F audio=@narrator.wav
curl http://127.0.0.1:2273/v1/audio/speech -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-tts-1.7b-base-q4_k_m","input":"Hello.","voice":"voice-0123456789ab"}' --output speech.wav
```

`consent` must be literally `true`: the creator attests that the voice is their own or that its speaker allowed it to be cloned. Names are trimmed, at most 48 characters, unique ignoring case, and cannot be a built-in voice name. Requests address a voice by its server-issued id (`voice-` and 12 hex digits), never by name or path. Custom voices appear in `/_localbase/models` speech metadata as extra `requestValues` with their names under `voice.custom`; `/v1/models` carries no voice list. A deleted or unknown id fails with HTTP 400 `voice_not_found`. The warm `tts-server` registers voices when it starts, so the first request for a new voice reloads it once in-flight generations finish (under a second with the weights cached). Logs record voice ids, never display names or audio, and diagnostics bundles record only the number of custom voices. The CLI manages the same store: `local-base voices list`, `local-base voices add clip.wav --name Narrator --consent` and `local-base voices remove <id>`.

Use [`local-base config`](docs/declarative-config.md) to validate, plan, apply, and export versioned TOML configuration without prompts.

Add your own models, or adjust built-in ones, in `<root>/catalog.d/`, or find and pin chat, transcription and image models from Hugging Face with `local-base models search`, `local-base models add hf:<org>/<repo>` or the catalog's **Find more models** view. New community models, plus verified-unsupported capabilities, measured memory peaks and multi-file install recipes for Hugging Face files, also arrive between releases through a remote catalog feed the gateway refreshes daily (`local-base models feed status`). See [User catalog and Hugging Face models](docs/user-catalog.md).

### Built-in catalog

The built-in catalog is a small, tested set: one entry per job, with a compact chat model and two larger ones. The table lists facts from each entry's pinned files; `local-base models show <id>` has the full record.

The planning minimum is LocalBase's figure for admission and fit: the accelerator demand at the reference launch (LLMs: 8,192-token context, 1 request at a time, q8_0 KV cache, or f16 for embeddings) plus a margin of 512 MiB or 10%, whichever is larger. "Measured" figures were recorded on Apple M-series 64 GiB unified memory, macOS 26 ([memory calibration](docs/memory-calibration.md)); "estimate" figures come from the formula. They are not hardware requirements: other settings and machines need different amounts, and `local-base models catalog` shows the figures this machine uses.

| Kind | Band or job | Model id | Pinned build | Planning minimum |
| --- | --- | --- | --- | --- |
| Chat | Compact | `qwen3.5-4b-q4_k_m` | Qwen3.5-4B, Q4_K_M GGUF, 262,144-token context, text only | 5 GB (measured) |
| Chat | Larger, mixture-of-experts | `qwen3.6-35b-a3b-iq4_nl_xl` | Qwen3.6-35B-A3B (35B, 3B active), IQ4_NL_XL GGUF and F16 vision projector, 262,144-token context, text and image input | 24 GB (estimate) |
| Chat | Larger, dense | `qwen3.8-27b-q4_k_m` | Qwen3.8-27B, Q4_K_M GGUF, 262,144-token context, text only | 21 GB (estimate) |
| Embeddings | Smaller | `qwen3-embedding-0.6b-q8_0` | Qwen3-Embedding-0.6B, Q8_0 GGUF, 1024 dimensions | 3 GB (estimate) |
| Embeddings | Larger | `qwen3-embedding-4b-q4_k_m` | Qwen3-Embedding-4B, Q4_K_M GGUF, 2560 dimensions | 5 GB (estimate) |
| Decisions | Default | `clef-flash-q4_k_m` | Cloudflare Clef Flash (9B), Q4_K_M GGUF and Q8_0 projector | 10 GB (measured) |
| Decisions | Larger | `clef-q4_k_m` | Cloudflare Clef (27B), Q4_K_M GGUF and Q8_0 projector | 25 GB (estimate) |
| Speech to text | Multilingual | `whisper-base-q8_0` | Whisper base (74M), ggml Q8_0 | 1 GB (estimate) |
| Speech to text | Multilingual, translation | `whisper-large-v3-turbo` | Whisper large-v3-turbo (809M), ggml F16 | 3 GB (measured) |
| Text to speech | Voices and cloning | `qwen3-tts-1.7b-base-q4_k_m` | Qwen3-TTS-12Hz-1.7B-Base, Q4_K_M GGUF | 5 GB (measured) |
| Text to speech | CPU (experimental) | `kokoro-82m-v1.0-q8` | Kokoro-82M, quantized ONNX, English | 1 GB (measured) |
| Image | Text to image | `z-image-turbo-q4_0` | Z-Image-Turbo (6B), Q4_0 GGUF, 8 steps | 15 GB (measured) |
| Image | Text to image | `flux2-klein-4b-q4_0` | FLUX.2-klein-4B, Q4_0 GGUF, 4 steps | 13 GB (measured) |
| Image | Text to image | `dreamshaper-8-lcm` | DreamShaper 8 LCM (Stable Diffusion 1.5), F16, 4 steps | 5 GB (measured) |
| Image | Image to image | `juggernaut-xl-v9` | Juggernaut XL v9 (SDXL 1.0 fine-tune), F16 | 16 GB (measured) |
| Video | Text and image to video (experimental) | `fastwan2.2-ti2v-5b-q6_k` | FastWan2.2-TI2V-5B, Q6_K GGUF, 480x832, 81 frames | 8 GB (measured) |
| Video | Speech to video (experimental, Linux x64 NVIDIA only) | `wan2.2-s2v-14b-fp8` | Wan2.2-S2V-14B, FP8 safetensors, 480x640, 33 frames | 21 GB (estimate) |

Models that earlier releases shipped and this one no longer lists keep working after an upgrade. When LocalBase finds one installed, or named in the model settings, it writes the same spec (id, pinned revision, files and checksums) to `<root>/catalog.d/` as your own entry, exactly as if you had added it from Hugging Face. API clients keep calling the same id, defaults and selections stay valid, and checked-in memory measurements still apply while the files are unchanged. The only visible difference is that it is no longer marked as qualified by LocalBase; its notes start with "No longer in the LocalBase catalog". Remove it like any user entry with `local-base models remove-entry <id>` after deselecting it.

Public `GET` and `HEAD /health/ready` report whether at least one configured modality can admit a request, including bounded queue waiting. `/health` remains process liveness.

### Image seeds, speech limits and transcription language

- `seed` on `/v1/images/generations` and `/v1/images/edits` is exact: the same seed, prompt and parameters give the same image. With `n` above 1, the images use `seed`, `seed + 1`, and so on.
- **With no `seed`, the API uses seed 42**, so the same prompt gives the same image. Send your own seed for variety. Chat and the browser playground send a random one when you leave it blank.
- Each item in `data` carries the `seed` it was made with. It is left out when it cannot be known: a JPEG or WebP generated with no `seed`.
- `/v1/audio/speech` takes up to 256 characters, and only `speed: 1`. The Qwen3 TTS runtime stops at 20.48 seconds of audio; text that needs longer is cut, and the response has the header `X-Localbase-Audio-Truncated: true`. A detached run carries the same fact as `result.notes: ["audio_truncated"]`. Split long text into shorter parts.
- `/v1/audio/transcriptions` and `/translations` with no `language` treat the audio as **English**, even on a multilingual model. That is whisper-server's own default. Foreign speech then comes back as confident, wrong English. Send `language=auto` to detect the language, or a code such as `es`. With `response_format=verbose_json`, `detected_language` still names what the model heard, so a mismatch with `language` is a warning sign. English-only (`.en`) models accept `language=auto` and any other code safely and stay English, marking foreign speech rather than transcribing it.

### Image and video inputs

A chat content part carries its content. `image_url.url` must be a `data:` URL, and `input_audio.data`, `file.file_data` and `file.file_id` must hold base64, an id or a `data:` URL. Anything else answers 400 `remote_media_not_allowed`, with `param` naming the first such field, and the whole request is refused, so a client that replays its history has to replace an address in an earlier message.

`POST /v1/images/edits` accepts multipart fields `model` (optional; defaults to the active image model), `prompt`, `image` or repeated `image[]` (1–4 PNG or JPEG files), optional nonempty PNG/JPEG `mask`, `n` (1–10), `size` (`256x256`, `512x512`, or `1024x1024`), `response_format` (`url` or `b64_json`), `output_format` (`png` or `jpeg`), and optional `strength` (0–1). Each input image and mask is structurally validated before model admission and limited to 2048 pixels per side and 4.2 megapixels. PNG decompression is bounded to its declared scanline size. WebP is not accepted because the pinned image backend uses stb_image, which does not decode WebP. Requests are limited to 20 MiB total and 8 MiB per file. Responses contain base64 image data for either response format.

`POST /v1/videos` retains its JSON request contract. For TI2V profiles, add top-level `input_reference` containing canonical PNG base64 or a `data:image/png;base64,...` URL; dimensions must match that profile's fixed canvas. Multipart requests may instead supply `input_reference` as a PNG file, with the remaining video fields as form values. Text-only TI2V requests remain supported. Image-to-video is experimental and unqualified; see [video qualification](docs/video-qualification.md).

### Decision models

Decision models answer structured questions about a state instead of generating text. `POST /v1/systemone` follows the llama.cpp System One contract:

```bash
curl http://127.0.0.1:2273/v1/systemone \
  -H "Authorization: Bearer $LOCALBASE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MODEL_ID",
    "state": { "invoice": "INV-104", "due": "2026-09-01", "paid": false },
    "questions": {
      "status": {
        "type": "choice",
        "instructions": "What is the invoice status?",
        "criteria": { "overdue": "Past the due date and unpaid", "paid": null, "open": null }
      },
      "urgency": { "type": "score", "criteria": ["low", "medium", "high"] },
      "escalate": { "type": "noul", "instructions": "Should a human follow up?" }
    }
  }'
```

- `state` is required and may be a string, object, array, or `null`.
- `files` and `images` are optional lists of `data:image/...;base64,` or `data:audio/...;base64,` URLs. Models without the matching projector return HTTP 501.
- Each question has a `type`, optional `instructions`, and `criteria`. `instructions` defaults to the question ID.
  - `choice`: 2–255 options mapped to a description or `null`.
  - `score`: 2–10 level labels, lowest first.
  - `noul`: optional `{ "true": "...", "false": "..." }` descriptions.
- Responses contain `model`, per-question `answers`, and `usage: { input_tokens, output_tokens }`. Choice answers return `choice`, `probabilities`, and `confidence`. Score answers return `score`, `legend`, `probabilities`, and `confidence`. Noul answers return the true probability as `noul`.

The default decision model is Clef Flash (`clef-flash-q4_k_m`, about 7.1 GB). It is Cloudflare's Apache-2.0 9B model, pinned to the ggml-org Q4_K_M GGUF and its Q8_0 image projector. Its decision launch (8,192-token context, 1 request at a time, Q8_0 projector) measured 8.62 GiB of accelerator demand on Apple M-series 64 GiB unified memory, macOS 26; other machines and settings differ:

```bash
local-base models install clef-flash-q4_k_m
```

Clef 27B (`clef-q4_k_m`, about 19.9 GB, Apache-2.0, post-trained from Qwen3.8-27B) is also built in. Its planning minimum is a 25 GB formula estimate at the same decision launch; it has not been measured. Add other decision GGUFs through the Hugging Face search. Files whose header carries decision heads are detected as decision models.

Decision models list `architecture.output_modalities: ["decisions"]` in `/v1/models`. They reject chat completions and embeddings, and other models reject `/v1/systemone` with HTTP 400 `unsupported_model_capability`. The route requires the `inference:decision` scope.

## Structured outputs

Chat completions accept OpenAI-compatible `response_format.type: "json_schema"` requests. LocalBase compiles the schema before starting a model and forwards the response format unchanged to the managed llama runtime. Schemas must use a root object, require every declared property, set `additionalProperties: false`, and fit within 10 nesting levels, 5,000 total properties, 1,000 local references, and 256 KiB. LocalBase also limits each object to 1,000 properties as a synchronous compilation resource bound; this is distinct from OpenAI's overall property limit.

Chat completions default to a maximum of 4096 generated tokens when neither `max_tokens` nor `max_completion_tokens` is provided. Set the environment-only `LOCALBASE_DEFAULT_MAX_TOKENS` variable to change this server default; it is capped at the selected model's effective per-slot context. Explicit client limits are passed through. Tool requests are accepted only for catalog models with the `tool-calling` feature.

The supported contract includes primitive and nullable types, strict object properties, object-valued array items, array bounds and string maximum lengths up to 10,000, safe integral integer bounds, scalar `enum` and `const` values matching their declared type, `anyOf`, and direct `#/$defs/name` or `#/definitions/name` references with ASCII identifier names. LocalBase rejects string minimum lengths, formats, remote or deeper references, other dialects, and keywords or combinations that the managed runtime would ignore.

For non-streaming responses, LocalBase validates ordinary assistant content when `finish_reason` is `stop`. A mismatch returns HTTP 502 with code `structured_output_validation_failed`. Refusals, tool calls, and truncated choices retain their original response semantics. Streaming remains incremental and relies on the runtime grammar rather than whole-response buffering.

See [Operating LocalBase](docs/operations.md) for queue limits, model-switch behavior, cancellation, telemetry, metadata, and compiled runtime qualification.

## Logs

`serve` is the single writer of redacted JSON Lines events under `$LOCALBASE_ROOT/logs`. The active file rotates at 10 MiB and retains five archives. These files are the primary operational record for foreground and managed services. A managed startup failure before the primary sink is available atomically records one private, bounded structured bootstrap event. On macOS, launchd discards stdout and appends stderr to the private `$LOCALBASE_ROOT/logs/service-stderr.log`, which catches what the structured sink cannot (Bun panics, uncaught errors, failures before logging starts); the managed gateway stops copying events to the console once its sink opens, and at startup a file over 1 MiB has its newest 1 MiB moved to `service-stderr.log.1` and is then emptied. It is raw, unredacted output, so `doctor` and `diagnostics` report only its size and path, never its content. The systemd journal remains the secondary Linux fallback.

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

Stored API keys default to `inference:chat`, `inference:embeddings`, `inference:decision`, `inference:image`, `inference:video`, `inference:speech`, `inference:transcription`, and `models:read`. Existing keys migrate to this set without administrative permissions or `inference:decision`; add that scope with `keys scopes`. The `LOCALBASE_API_KEY` environment credential retains full access.

```bash
local-base keys create --name chat-client --scopes inference:chat,models:read --json
local-base keys list --json
local-base keys scopes key_ID --scopes inference:video,models:read --json
local-base keys rotate key_ID --json
local-base keys revoke key_ID --json
```

`keys scopes` replaces the complete scope set and takes effect on the next request without a restart. Use `--scopes ""` to grant no permissions. Scope lists accept only these permissions:

```text
inference:chat, inference:embeddings, inference:decision, inference:image,
inference:video, inference:speech, inference:transcription, models:read,
models:manage, configuration:read, configuration:manage, keys:read,
keys:manage, access:read, access:manage, sessions:read, sessions:revoke,
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

