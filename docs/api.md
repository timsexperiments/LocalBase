# API overview

The gateway listens on `http://127.0.0.1:2273` by default. Send an API key from `local-base keys create` as `Authorization: Bearer <key>`. Errors use the OpenAI envelope: `{"error":{"message","type","param","code"}}`.

## OpenAI-compatible

| Route | Purpose | Scope |
| --- | --- | --- |
| `GET /v1/models`, `GET /v1/models/{id}` | List models, or read one | `models:read` |
| `POST /v1/chat/completions` | Chat, streaming, tools and structured outputs | `inference:chat` |
| `POST /v1/embeddings` | Embeddings | `inference:embeddings` |
| `POST /v1/audio/transcriptions`, `POST /v1/audio/translations` | Speech to text | `inference:transcription` |
| `POST /v1/audio/speech` | Text to speech | `inference:speech` |
| `POST /v1/images/generations`, `POST /v1/images/edits` | Images | `inference:image` |
| `POST /v1/videos`, `GET /v1/videos/{id}`, `.../content`, `.../cancel` | Video jobs | `inference:video` |

## LocalBase additions

| Route | Purpose |
| --- | --- |
| `POST /v1/systemone` | Decisions: structured answers about a state (`inference:decision`) |
| `GET /health`, `GET /health/ready` | Liveness and readiness, without credentials |
| `/_localbase/models` and below | Model details, install and unload |
| `/_localbase/runs` and below | Detached runs |
| `/_localbase/api-keys`, `/_localbase/access-management`, `/_localbase/voices`, `/_localbase/backends`, `/_localbase/model-imports` | Administration |

`/_localbase/` routes aren't part of the stable `/v1` API and can change between releases.

See [API details](api-details.md) for request and response contracts, and `go/DESIGN.md` §21 for detached runs.
