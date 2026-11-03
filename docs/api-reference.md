# API reference

<!-- Generated from go/internal/gateway/openapi.json. Do not edit this page: change the document, then run `go run ./tools/openapi-docs` from go/. -->

`/v1` is the stable API. `/_localbase` is for administering one installation, and can change between releases. Errors use the OpenAI envelope; branch on `error.code`. A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with an API key, and the gateway sends no CORS headers, so a key cannot be used from a web page.

A running gateway serves this API's description at `GET /openapi.json`, and this page is generated from it.

For how to use it, see the [API overview](api.md).

## Inference

The OpenAI-compatible API, plus decisions.

### `POST /v1/audio/speech`

Text to speech.

**Needs:** `inference:speech`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `input` | string | yes |  |
| `model` | string | yes |  |
| `response_format` | `wav`, `pcm` |  | Defaults to `wav`, unlike OpenAI. |
| `speed` | `1` |  | Only 1 is accepted. |
| `stream_format` | `audio`, `sse` |  |  |
| `voice` | string | yes | `default`, `harbor`, `willow`, or a custom voice id (`voice-` and 12 hex). |

**Responses:**

- `200`: Audio. A long text can be cut at the runtime's limit, and then `X-Localbase-Audio-Truncated: true` says so.
- `400`: The request is invalid: `validation_failed` and more specific codes, or `voice_not_supported` when this model can't use that voice.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/audio/transcriptions`

Speech to text.

**Needs:** `inference:transcription`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `file` | string | yes | The audio. |
| `include[]` | list of string |  |  |
| `language` | string |  | With none, audio is treated as English, even on a multilingual model. Send `auto` to detect, or a code such as `es`. |
| `model` | string |  |  |
| `prompt` | string |  |  |
| `response_format` | `json`, `text`, `srt`, `verbose_json`, `vtt` |  |  |
| `temperature` | number |  |  |
| `timestamp_granularities[]` | list of `word`, `segment` |  |  |

**Responses:**

- `200`: The transcript, in the format asked for.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/audio/translations`

Speech to English text.

**Needs:** `inference:transcription`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `file` | string | yes | The audio. |
| `include[]` | list of string |  |  |
| `language` | string |  | With none, audio is treated as English, even on a multilingual model. Send `auto` to detect, or a code such as `es`. |
| `model` | string |  |  |
| `prompt` | string |  |  |
| `response_format` | `json`, `text`, `srt`, `verbose_json`, `vtt` |  |  |
| `temperature` | number |  |  |
| `timestamp_granularities[]` | list of `word`, `segment` |  |  |

**Responses:**

- `200`: The transcript, in the format asked for.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/chat/completions`

Chat, streaming, tools and structured output.

A content part carries its content: one that names an address answers 400 `remote_media_not_allowed`, with `param` naming the first such field.

**Needs:** `inference:chat`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `frequency_penalty` | number |  |  |
| `logit_bias` | object or null |  |  |
| `max_completion_tokens` | number |  |  |
| `max_tokens` | number |  | Defaults to 4096 when neither limit is sent, capped at the context available. |
| `messages` | list of ChatMessage | yes |  |
| `model` | string | yes | A model id from `GET /v1/models`. Required: there is no default for API callers. |
| `n` | number |  |  |
| `presence_penalty` | number |  |  |
| `response_format` | object or object |  |  |
| `stop` | string or list of string or null |  |  |
| `stream` | boolean |  |  |
| `temperature` | number |  |  |
| `tool_choice` | `none`, `auto`, `required` or object |  |  |
| `tools` | list of object |  |  |
| `top_p` | number |  |  |
| `user` | string |  |  |

**Responses:**

- `200`: A completion, or with `stream: true` server-sent events ending in `data: [DONE]`.
- `400`: The request is invalid: `validation_failed` and more specific codes, or `context_length_exceeded` when the input is more than one slot of the model's context can hold.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `502`: The backend failed or answered something unusable (`upstream_error`), or a finished reply didn't match the requested JSON Schema (`structured_output_validation_failed`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/embeddings`

Embeddings.

**Needs:** `inference:embeddings`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `dimensions` | integer |  | Within what the model supports. |
| `encoding_format` | `float` |  |  |
| `input` | string or list of string or list of number or list of list of number | yes | Text, a list of texts, tokens, or lists of tokens. |
| `model` | string | yes |  |
| `user` | string |  |  |

**Responses:**

- `200`: The vectors.
- `400`: The request is invalid: `validation_failed` and more specific codes, or `context_length_exceeded` when the input is more than one slot of the model's context can hold.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/images/edits`

Image to image.

**Needs:** `inference:image`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `image` | string |  | A PNG or JPEG, or repeat `image[]` for up to 4. |
| `mask` | string |  |  |
| `model` | string |  |  |
| `n` | number |  |  |
| `prompt` | string | yes |  |
| `response_format` | `url`, `b64_json` |  |  |
| `size` | `256x256`, `512x512`, `1024x1024` |  |  |

**Responses:**

- `200`: The images.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/images/generations`

Text to image.

**Needs:** `inference:image`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `cfg_scale` | number |  |  |
| `lora_strength` | number |  |  |
| `model` | string |  | Defaults to the active image model. |
| `n` | number |  |  |
| `negative_prompt` | string |  |  |
| `prompt` | string | yes |  |
| `quality` | `standard`, `hd` |  |  |
| `response_format` | `url`, `b64_json` |  |  |
| `seed` | integer |  | Exact. With none, the API uses 42, so the same prompt gives the same image. |
| `size` | `256x256`, `512x512`, `1024x1024` |  | Sizes the model supports. |
| `steps` | integer |  |  |
| `style` | `vivid`, `natural` |  |  |
| `user` | string |  |  |

**Responses:**

- `200`: The images.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/systemone`

Structured answers about a state.

A LocalBase route for decision models: classification, scoring and yes/no in one forward pass. Chat models answer 400 `unsupported_model_capability`.

**Needs:** `inference:decision`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `files` | list of string |  | `data:image/…;base64,` or `data:audio/…;base64,` URLs. |
| `images` | list of string |  | The same as files. |
| `model` | string | yes |  |
| `questions` | object | yes | By question id; `instructions` defaults to the id. |
| `state` | string or object or array or null |  | What the questions are about. |

**Responses:**

- `200`: The answers.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: The queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: The model lacks the projector for the files sent.
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /v1/videos`

Start a video job.

Answers 202 with the job. A job that cannot fit fails with `insufficient_memory`, rather than the request being refused.

**Needs:** `inference:video`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `fps` | integer |  |  |
| `frames` | integer |  |  |
| `height` | integer |  |  |
| `input` | object or object or object | yes |  |
| `input_reference` | string |  | PNG base64, only with `input.kind` text. |
| `model` | string | yes |  |
| `negative_prompt` | string |  |  |
| `prompt` | string | yes |  |
| `width` | integer |  |  |

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `input_reference` | string |  |  |

**Responses:**

- `202`: Accepted.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: A video job is already running (`video_job_busy`), or the queue is full or the wait timed out (`inference_queue_full`, `inference_queue_timeout`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /v1/videos/{job_id}`

A job's state.

**Needs:** `inference:video`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `job_id` | path | string | yes | The job's id. |

**Responses:**

- `200`: The job. A failed job's `error.code` is `video_generation_failed`, or says why it couldn't run: `insufficient_memory`, `model_install_consent_required`, `model_hardware_ineligible` or `experimental_model_not_allowed`.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such job (`video_job_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

### `DELETE /v1/videos/{job_id}`

Delete a finished job and its file.

**Needs:** `inference:video`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `job_id` | path | string | yes | The job's id. |

**Responses:**

- `204`: Deleted.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).

### `POST /v1/videos/{job_id}/cancel`

Cancel a job.

**Needs:** `inference:video`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `job_id` | path | string | yes | The job's id. |

**Responses:**

- `200`: The job, as it now stands.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such job (`video_job_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

### `GET /v1/videos/{job_id}/content`

A finished job's video.

**Needs:** `inference:video`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `job_id` | path | string | yes | The job's id. |

**Responses:**

- `200`: The video.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such job (`video_job_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

## Models

What can be asked for.

### `GET /v1/models`

List the models this key may use.

**Needs:** `models:read`.

**Responses:**

- `200`: Per kind: the active model and the selected ones, filtered to what this key may invoke.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

### `GET /v1/models/{model}`

Read one model.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model` | path | string | yes | A model id from `GET /v1/models`. |

**Responses:**

- `200`: One entry of the list.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

## Health

Liveness, readiness and this document.

### `GET /health`

Liveness.

**Responses:**

- `200`: The process is up.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: The gateway is stopping.

### `HEAD /health`

Liveness, status only.

**Responses:**

- `200`: The process is up.
- `503`: The gateway is stopping.

### `GET /health/ready`

Whether any modality can admit a request.

**Responses:**

- `200`: Some modality admits requests.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Nothing can admit a request now.

### `HEAD /health/ready`

Readiness, status only.

**Responses:**

- `200`: Some modality admits requests.
- `503`: Nothing can admit a request now.

### `GET /openapi.json`

This document.

The same static text on every gateway, except for `info.version`. It holds nothing about this installation.

**Responses:**

- `200`: The OpenAPI document.
- `304`: Unchanged (`If-None-Match`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.

### `HEAD /openapi.json`

This document, headers only.

**Responses:**

- `200`: The document exists; `ETag` says which.
- `304`: Unchanged (`If-None-Match`).

## Model management

The catalog, what is installed and loaded, and how models run.

### `GET /_localbase/auth-profiles`

Download credential profiles.

Ids, origins and header names. Header values never leave the credential file.

**Needs:** `models:manage`.

**Responses:**

- `200`: The profiles.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/backends`

The registered backends, and this device.

**Needs:** `models:read`.

**Responses:**

- `200`: Every backend and how it stands here.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/default-chat-models`

The default chat models, and the one in force.

**Needs:** `models:read`.

**Responses:**

- `200`: The list and the effective choice.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/default-chat-models`

Set the default chat models.

One id makes that model the active one. Each must be an enabled chat model.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 4096 bytes.

| Field | Type | Required | About |
|---|---|---|---|
| `models` | list of string | yes | First choice first, each once. |

**Responses:**

- `200`: The list now in force.
- `400`: Invalid (`validation_failed`), or a model does not qualify (`invalid_request`).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/hf-token`

Whether a Hugging Face token is set.

**Needs:** `models:manage`.

**Responses:**

- `200`: The status.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/hf-token`

Save, clear or check the Hugging Face token.

`save` checks the token with the Hub first, unless `validate` is false. A token the Hub rejects is not saved.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 1024 bytes.

- `action: save`: `token` (required), `validate`
- One of: `action` (required)

**Responses:**

- `200`: The status, with `check` when the Hub was asked.
- `400`: Invalid (`validation_failed`), or the Hub rejected the token (`hf_token_invalid`).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: `validate`: no token is saved or set (`hf_token_missing`).
- `413`: The body is over this route's limit.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/model-backend`

What runs a model here.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model` | query | string |  | The model's id. |

**Responses:**

- `200`: The selection, and the automatic result.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/model-backend`

Choose a model's backend, or go back to automatic.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 64 KiB.

- `action: set`: `backendId` (required), `model` (required), `variantId`
- `action: reset`: `backendId`, `model` (required), `variantId`

**Responses:**

- `200`: The selection now in force.
- `400`: Invalid. A body that is not JSON answers in the management shape (`validation_failed`), anything else in the OpenAI envelope.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `413`: The body is over the 64 KiB limit (`payload_too_large`).
- `499`: The client went away while sending the body (`request_aborted`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/model-discovery`

Search the Hugging Face Hub, preview a repository, add one or install a recipe.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 2048 bytes.

- `action: search`: `kind`, `query`
- `action: preview`: `repo` (required)
- `action: add`: `install`, `modelId`, `repo` (required), `selector`
- `action: install-recipe`: `modelId` (required)

**Responses:**

- `200`: The action's answer.
- `400`: Invalid (`validation_failed`), no recipe (`no_recipe`), or the Hub refused it (`hub_rejected`, `hub_auth_required`, `hub_rate_limited`).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: Not in this catalog (`model_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: A built-in or remote entry owns that id (`catalog_conflict`).
- `413`: The body is over this route's limit.
- `502`: The Hub could not be reached or answered badly (`hub_unavailable`, `hub_auth_required`, `hub_rate_limited`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/model-imports`

An import's progress.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `id` | query | string |  | The job id from starting it. |

**Responses:**

- `200`: The job.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No import with that id (`import_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/model-imports`

Resolve an import source, start the import, or cancel it.

Cancelling differs from cancelling an install, on purpose. An import is identified by its `job`, and cancelling removes what it downloaded, because its source may change between tries. An install is identified by `modelId` and keeps its partial file, because a pinned checksum makes resuming safe.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 64 KiB.

- One of: `action` (required), `allowHttp`, `authProfile`, `backend`, `confirmCustomSource`, `files`, `id`, `name`, `revision`, `sha256`, `source` (required), `task`
- `action: cancel`: `job` (required)

**Responses:**

- `200`: `resolve`: what would be downloaded. `cancel`: the job, usually still `running` for a moment.
- `202`: `start`: the import is running.
- `400`: Invalid. A body that is not JSON answers in the management shape (`validation_failed`). Anything else answers in the OpenAI envelope: `validation_failed`, `import_unresolved`, `import_choose_file`, `custom_source_unconfirmed` or `import_task_required`.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: `cancel`: no import with that id (`import_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `413`: The body is over the 64 KiB limit (`payload_too_large`).
- `499`: The client went away while sending the body (`request_aborted`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/model-management`

Install state of each catalog model.

**Needs:** `models:read`.

**Responses:**

- `200`: The models and the operation last run on each.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/model-management`

Install, uninstall, enable, disable or activate a model, or cancel its install.

One install runs at a time, in the background. `GET` reports its progress.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 1024 bytes.

| Field | Type | Required | About |
|---|---|---|---|
| `action` | `install`, `uninstall`, `enable`, `disable`, `activate`, `cancel` | yes |  |
| `modelId` | string | yes |  |

**Responses:**

- `200`: Done. A cancelled install answers here with state `cancelled`. A cancel with no install of this model running answers here too, with the operation recorded for it, unchanged, so a retry is safe.
- `202`: An install was accepted and is running, or a cancel has not finished after 5 s, so its state is still `running`.
- `400`: Invalid (`validation_failed`), or not allowed for this model (`invalid_request`).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: Not possible now: `conflict` (including a cancel with no install of this model running and no operation recorded for it), `unsafe_path`, `storage_unavailable` or `insufficient_storage`.
- `413`: The body is over this route's limit.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/models`

The catalog, with this device's state.

Unknown filter values are dropped.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `capability` | query | string |  | Comma-separated capability ids. A model must have them all. |
| `fit` | query | string |  | Comma-separated verdicts: `fits`, `tight`, `wont-fit`. |
| `installed` | query | `true`, `false` |  | Only installed, or only not installed. |
| `kind` | query | string |  | Comma-separated kinds: `llm`, `stt`, `tts`, `image`, `video`. |
| `license` | query | string |  | Comma-separated: `open`, `conditional`, `prohibited`. |
| `q` | query | string |  | Words to find in the id, name and summary. |
| `reverse` | query | `true`, `false` |  | Reverse the order. |
| `sort` | query | `name`, `size`, `memory`, `fit`, `context` |  | Without one, the catalog's order is kept. |

**Responses:**

- `200`: The models that match.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/models/{model_id}`

One catalog model.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`. |

**Responses:**

- `200`: The model.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/models/{model_id}/admission`

What a request for this model would meet now.

The real admission's verdict and refusal. It starts, stops and installs nothing. A model can pass and still fail to start.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`, or an imported model's id. |
| `assumeInstalled` | query | `1` |  | `1` answers the memory question alone, for a model not yet on disk. Its demand is then an estimate. |

**Responses:**

- `200`: The verdict, whichever way it went.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/models/{model_id}/placement`

A model's CPU and GPU placement.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`, or an imported model's id. |

**Responses:**

- `200`: The stored choice, the loaded split and the next load's.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: Unknown model (`model_not_found`), or planning is off on this gateway (`route_disabled`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `PUT /_localbase/models/{model_id}/placement`

Choose a model's placement.

It applies on the next load and never restarts the model. An unknown model answers 404 before the permission is checked.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`, or an imported model's id. |

**Request:** `application/json`. At most 4096 bytes.

| Field | Type | Required | About |
|---|---|---|---|
| `contextSize` | integer |  | Sets the shared context size for all models. |
| `cpuMoeLayers` | integer |  | `custom` only: layers whose experts stay in system memory. |
| `gpuLayers` | integer |  | `custom` only. |
| `mode` | `auto`, `gpu`, `cpu`, `custom` |  | Defaults to `auto`. |

**Responses:**

- `200`: The stored choice, the loaded split and the next load's.
- `400`: Invalid (`validation_failed`), or this device or backend cannot honour it (`placement_unsupported`).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: Unknown model (`model_not_found`), or planning is off on this gateway (`route_disabled`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: The model is loading (`model_loading`), or this cannot fit (`placement_does_not_fit`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `DELETE /_localbase/models/{model_id}/placement`

Go back to automatic placement.

It applies on the next load and never restarts the model. An unknown model answers 404 before the permission is checked.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`, or an imported model's id. |

**Responses:**

- `200`: The stored choice, the loaded split and the next load's.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: Unknown model (`model_not_found`), or planning is off on this gateway (`route_disabled`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: The model is loading (`model_loading`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/models/{model_id}/unload`

Unload a loaded model.

Requests holding the model finish first. Its queued requests are refused, and a later request loads it again.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `model_id` | path | string | yes | A catalog id from `GET /_localbase/models`, or an imported model's id. |
| `wait` | query | `true`, `false` |  | `true` (the default) drains for up to 30 s. `false` refuses at once if anything is in flight. |

**Responses:**

- `200`: Unloaded.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: Unknown (`model_not_found`) or not loaded (`model_not_loaded`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: Still busy: `model_busy`. The model keeps running.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Runs

Generation that outlives the connection that started it.

### `GET /_localbase/runs`

The caller's runs.

Newest first, with no prompt or output text.

**Needs:** `any`.

**Responses:**

- `200`: The runs.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/audio/speech`

Start a text-to-speech run.

The `/v1/audio/speech` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:speech`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `input` | string | yes |  |
| `model` | string | yes |  |
| `response_format` | `wav`, `pcm` |  | Defaults to `wav`, unlike OpenAI. |
| `speed` | `1` |  | Only 1 is accepted. |
| `stream_format` | `audio`, `sse` |  |  |
| `voice` | string | yes | `default`, `harbor`, `willow`, or a custom voice id (`voice-` and 12 hex). |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/audio/transcriptions`

Start a speech-to-text run.

The `/v1/audio/transcriptions` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:transcription`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `file` | string | yes | The audio. |
| `include[]` | list of string |  |  |
| `language` | string |  | With none, audio is treated as English, even on a multilingual model. Send `auto` to detect, or a code such as `es`. |
| `model` | string |  |  |
| `prompt` | string |  |  |
| `response_format` | `json`, `text`, `srt`, `verbose_json`, `vtt` |  |  |
| `temperature` | number |  |  |
| `timestamp_granularities[]` | list of `word`, `segment` |  |  |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/audio/translations`

Start a speech-to-English run.

The `/v1/audio/translations` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:transcription`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `file` | string | yes | The audio. |
| `include[]` | list of string |  |  |
| `language` | string |  | With none, audio is treated as English, even on a multilingual model. Send `auto` to detect, or a code such as `es`. |
| `model` | string |  |  |
| `prompt` | string |  |  |
| `response_format` | `json`, `text`, `srt`, `verbose_json`, `vtt` |  |  |
| `temperature` | number |  |  |
| `timestamp_granularities[]` | list of `word`, `segment` |  |  |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/chat/completions`

Start a chat run.

The `/v1/chat/completions` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:chat`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `frequency_penalty` | number |  |  |
| `logit_bias` | object or null |  |  |
| `max_completion_tokens` | number |  |  |
| `max_tokens` | number |  | Defaults to 4096 when neither limit is sent, capped at the context available. |
| `messages` | list of ChatMessage | yes |  |
| `model` | string | yes | A model id from `GET /v1/models`. Required: there is no default for API callers. |
| `n` | number |  |  |
| `presence_penalty` | number |  |  |
| `response_format` | object or object |  |  |
| `stop` | string or list of string or null |  |  |
| `stream` | boolean |  |  |
| `temperature` | number |  |  |
| `tool_choice` | `none`, `auto`, `required` or object |  |  |
| `tools` | list of object |  |  |
| `top_p` | number |  |  |
| `user` | string |  |  |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `499`: The client went away while sending the body. There is no body.
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/images/edits`

Start an image-to-image run.

The `/v1/images/edits` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:image`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `image` | string |  | A PNG or JPEG, or repeat `image[]` for up to 4. |
| `mask` | string |  |  |
| `model` | string |  |  |
| `n` | number |  |  |
| `prompt` | string | yes |  |
| `response_format` | `url`, `b64_json` |  |  |
| `size` | `256x256`, `512x512`, `1024x1024` |  |  |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/images/generations`

Start a text-to-image run.

The `/v1/images/generations` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:image`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `cfg_scale` | number |  |  |
| `lora_strength` | number |  |  |
| `model` | string |  | Defaults to the active image model. |
| `n` | number |  |  |
| `negative_prompt` | string |  |  |
| `prompt` | string | yes |  |
| `quality` | `standard`, `hd` |  |  |
| `response_format` | `url`, `b64_json` |  |  |
| `seed` | integer |  | Exact. With none, the API uses 42, so the same prompt gives the same image. |
| `size` | `256x256`, `512x512`, `1024x1024` |  | Sizes the model supports. |
| `steps` | integer |  |  |
| `style` | `vivid`, `natural` |  |  |
| `user` | string |  |  |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/v1/systemone`

Start a decision run.

The `/v1/systemone` body, at most 32 MiB, and that route's permission. It runs on the server, independent of this connection. A request the route refuses before queueing gets that route's status and error, and no run exists.

**Needs:** `inference:decision`.

**Request:** `application/json`.

| Field | Type | Required | About |
|---|---|---|---|
| `files` | list of string |  | `data:image/…;base64,` or `data:audio/…;base64,` URLs. |
| `images` | list of string |  | The same as files. |
| `model` | string | yes |  |
| `questions` | object | yes | By question id; `instructions` defaults to the id. |
| `state` | string or object or array or null |  | What the questions are about. |

**Responses:**

- `202`: Accepted. Follow it at `Location`.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such thing (`model_not_found`, or the route's own code).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: It cannot run as configured, and retrying will not help: `model_install_consent_required`, `backend_not_selected`, `backend_unavailable`, `model_assets_missing`.
- `413`: The body is over this route's limit.
- `422`: This model can never fit on this computer, even idle (`model_too_large`). Retrying will not help.
- `429`: Too many active runs (`too_many_runs`), or a `/v1` refusal.
- `501`: The model lacks the projector for the files sent.
- `502`: The backend failed or answered something unusable (`upstream_error`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/runs/{run_id}`

A run, with its output so far.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `run_id` | path | string | yes | `run_` and 32 hex digits, from creating a run. |

**Responses:**

- `200`: The run.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `404`: No such run (`run_not_found`). Another caller's run answers the same, and this comes before the method is checked.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `DELETE /_localbase/runs/{run_id}`

Dismiss a run.

Stops it if active, then deletes it and its checkpoint.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `run_id` | path | string | yes | `run_` and 32 hex digits, from creating a run. |

**Responses:**

- `200`: Deleted.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `404`: No such run (`run_not_found`). Another caller's run answers the same, and this comes before the method is checked.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/runs/{run_id}/cancel`

Stop a run.

The only thing that cancels a run. Stopping a finished run changes nothing.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `run_id` | path | string | yes | `run_` and 32 hex digits, from creating a run. |

**Responses:**

- `200`: The run, with partial output kept.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `404`: No such run (`run_not_found`). Another caller's run answers the same, and this comes before the method is checked.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/runs/{run_id}/events`

Follow a run.

Everything after `Last-Event-ID` or `after`, then live, until the `end` event. Events are `status`, `chunk`, `error`, `snapshot` and `end`. A restarted gateway sends only `snapshot` and `end`.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `Last-Event-ID` | header | string |  | The last event id seen. |
| `run_id` | path | string | yes | `run_` and 32 hex digits, from creating a run. |
| `after` | query | integer |  | The last event id seen. `Last-Event-ID` takes precedence. |

**Responses:**

- `200`: Server-sent events.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `404`: No such run (`run_not_found`). Another caller's run answers the same, and this comes before the method is checked.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/runs/{run_id}/result`

A completed media run's result.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `run_id` | path | string | yes | `run_` and 32 hex digits, from creating a run. |

**Responses:**

- `200`: The `/v1` response body, in its own content type.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: A `--no-auth` gateway refused the request (`no_auth_request_refused`). Any authenticated caller is otherwise admitted.
- `404`: No such run (`run_not_found`). Another caller's run answers the same, and this comes before the method is checked.
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: No result to fetch: not completed, not a media run, or the gateway restarted (`run_result_unavailable`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Access

Who may sign in from a browser, and with what permissions.

### `GET /_localbase/access-management`

Browser access settings, policy and users.

**Needs:** `access:read`.

**Responses:**

- `200`: Everything, without secrets.
- `401`: The key is missing or not accepted (`invalid_api_key`).
- `403`: The key lacks the permission (`insufficient_permissions`, in the management shape). A key can't give out more than it holds (`exceeds_caller_permissions`, `exceeds_caller_expiry`). A `--no-auth` gateway refusing the request answers in the OpenAI envelope (`no_auth_request_refused`).
- `405`: Wrong method. There is no body; see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/access-management`

Change browser access, a policy or a user.

`test-policy` needs `access:read`, and every other action `access:manage`. The body is checked before the action's permission, so either permission gets 400 for an invalid body.

**Needs:** `access:manage`.

**Request:** `application/json`. At most 64 KiB, with exactly the action's fields.

- `action: configure-cloudflare`: `origin` (required), `permissions` (required), `provider` (required)
- `action: upsert-oidc`: `origin` (required), `permissions` (required), `registration` (required)
- `action: upsert-github`: `origin` (required), `permissions` (required), `registration` (required)
- `action: upsert-magic-link`: `origin` (required), `permissions` (required), `registration` (required)
- `action: remove-registration`: `registrationId` (required)
- `action: disable`
- `action: configure-email-delivery`: `config` (required)
- `action: disable-email-delivery`
- `action: test-email-delivery`: `to` (required)
- `action: invite-user`: `email` (required), `issuer`, `roles` (required), `sendEmail`
- `action: replace-user-roles`: `email` (required), `roles` (required)
- `action: enable-user`: `email` (required)
- `action: disable-user`: `email` (required)
- `action: remove-user`: `email` (required)
- `action: apply-policy`: `expectedPolicyRevision` (required), `policy` (required)
- `action: clear-policy`: `expectedPolicyRevision` (required)
- `action: test-policy`: `identity` (required)

**Responses:**

- `200`: The action's answer.
- `201`: `invite-user`.
- `400`: The request is invalid: `validation_failed`, `role_not_found`, or a binding with no sign-in provider or an unknown one (`issuer_required`, `issuer_unknown`).
- `401`: The key is missing or not accepted (`invalid_api_key`).
- `403`: The key lacks the permission (`insufficient_permissions`, in the management shape). A key can't give out more than it holds (`exceeds_caller_permissions`, `exceeds_caller_expiry`). A `--no-auth` gateway refusing the request answers in the OpenAI envelope (`no_auth_request_refused`).
- `404`: Not found: `registration_not_found` or `managed_user_not_found`.
- `405`: Wrong method. There is no body; see the `Allow` header.
- `409`: Not possible now: `provider_not_configured`, `policy_not_configured`, `email_delivery_not_configured`, `email_delivery_in_use`, `managed_user_exists`, `policy_conflict`, `policy_revision_conflict` or `would_lock_out_admins`.
- `413`: The body is over the 64 KiB limit (`payload_too_large`).
- `499`: The client went away while sending the body (`request_aborted`).
- `502`: The email could not be sent (`email_delivery_failed`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/local-sessions`

Local sign-in sessions.

**Needs:** `sessions:read`.

**Responses:**

- `200`: Live sessions, most recently used first. Never a token.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/local-sessions`

End one local session, or all of them.

**Needs:** `sessions:revoke`.

**Request:** `application/json`. At most 1024 bytes.

- `action: revoke`: `id` (required)
- `action: revoke-all`: `id`

**Responses:**

- `200`: Done.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Keys

Scoped API keys.

### `GET /_localbase/api-keys`

List API keys.

**Needs:** `keys:read`.

**Responses:**

- `200`: The keys.
- `401`: The key is missing or not accepted (`invalid_api_key`).
- `403`: The key lacks the permission (`insufficient_permissions`, in the management shape). A key can't give out more than it holds (`exceeds_caller_permissions`, `exceeds_caller_expiry`). A `--no-auth` gateway refusing the request answers in the OpenAI envelope (`no_auth_request_refused`).
- `405`: Wrong method. There is no body; see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/api-keys`

Create, rotate, revoke or rescope a key.

You can't revoke the key you are using or remove `access:manage` from it, and the last holder of `access:manage` can't lose it.

**Needs:** `keys:manage`.

**Request:** `application/json`. At most 64 KiB.

- `action: create`: `expiresDays`, `name` (required), `scopes` (required)
- One of: `action` (required), `keyId` (required)
- `action: set-scopes`: `keyId` (required), `scopes` (required)

**Responses:**

- `200`: `rotate` has the new secret; `revoke` and `set-scopes` do not.
- `201`: `create`.
- `400`: The request is invalid (`validation_failed`).
- `401`: The key is missing or not accepted (`invalid_api_key`).
- `403`: The key lacks the permission (`insufficient_permissions`, in the management shape). A key can't give out more than it holds (`exceeds_caller_permissions`, `exceeds_caller_expiry`). A `--no-auth` gateway refusing the request answers in the OpenAI envelope (`no_auth_request_refused`).
- `404`: No such key (`key_not_found`).
- `405`: Wrong method. There is no body; see the `Allow` header.
- `409`: This would lock someone out (`would_lock_out_admins`).
- `413`: The body is over the 64 KiB limit (`payload_too_large`).
- `499`: The client went away while sending the body (`request_aborted`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Host

What this computer is doing.

### `PUT /_localbase/computer-name`

Name this computer.

**Needs:** `models:manage`.

**Request:** `application/json`. At most 1024 bytes.

| Field | Type | Required | About |
|---|---|---|---|
| `name` | string | yes | Trimmed, then at most 64 characters, with no control characters. Empty goes back to the machine's name. |

**Responses:**

- `200`: The name now in force.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/host`

What this computer is doing.

Read-only, and it starts nothing. Figures come from one shared sample a second.

**Needs:** `models:read`.

**Responses:**

- `200`: The memory and activity figures.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/update`

Whether a newer version is known.

Reads what is stored, and makes no request. The check is off by default.

**Needs:** `models:read`.

**Responses:**

- `200`: What is stored.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).

### `POST /_localbase/update`

Turn the daily check on or off.

`access:manage` also works. On, the gateway makes one request a day to GitHub, sending nothing about this computer. Nothing here installs anything.

**Needs:** `system:manage` or `[access:manage]`.

**Request:** `application/json`. At most 4 KiB.

| Field | Type | Required | About |
|---|---|---|---|
| `action` | `set-check` | yes |  |
| `enabled` | boolean | yes |  |

**Responses:**

- `200`: Done.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `413`: The body is over this route's limit.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/usage`

Requests, tokens, errors and latency, as hourly totals.

Any authenticated caller sees its own usage. Asking for `scope=all`, or naming another principal, needs `models:manage` or `system:read`, and answers 403 otherwise, not an empty result.

**Needs:** `any`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `from` | query | string |  | Start, RFC 3339, instead of `range`. |
| `group` | query | list of `principal`, `model`, `route` |  | Break down by these. Repeatable. |
| `model` | query | string |  | One model's id. |
| `principal` | query | string |  | One principal's id. Naming another needs `scope=all`. |
| `range` | query | `24h`, `7d`, `30d` |  | How far back. Defaults to `7d`. |
| `route` | query | `chat`, `embeddings`, `decision`, `speech`, `transcription`, `translation`, `image`, `image-edit`, `video` |  | One route. |
| `scope` | query | `self`, `all` |  | `self` is your own usage, the default. `all` needs `models:manage` or `system:read`. |
| `to` | query | string |  | End, RFC 3339, instead of `range`. |

**Responses:**

- `200`: The totals, including counts not yet written.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/usage`

Clear the totals, or change retention or recording.

`access:manage` also works, since it can grant itself `system:manage` anyway. This machine's CLI can use the instance token from the root's lock file, on a request from loopback. Clearing removes the counts in memory too.

**Needs:** `system:manage` or `[access:manage]`.

**Request:** `application/json`. At most 4 KiB.

- `action: clear`: `before`
- `action: set-retention`: `days` (required)
- `action: set-recording`: `enabled` (required)

**Responses:**

- `200`: Done.
- `400`: The request is invalid (`validation_failed` and more specific codes).
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `413`: The body is over this route's limit.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Voices

Custom speech voices, each from a consented recording.

### `GET /_localbase/voices`

Custom voices, limits and the consent statement.

**Needs:** `models:read`.

**Responses:**

- `200`: The voices.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `POST /_localbase/voices`

Add a voice from a reference recording.

**Needs:** `models:manage`.

**Request:** `multipart/form-data`.

| Field | Type | Required | About |
|---|---|---|---|
| `audio` | string | yes | A WAV of speech. Only its bytes count, not its filename or declared type. |
| `consent` | `true` | yes | You have the right to use this voice. |
| `name` | string | yes | Normalized and trimmed. No control characters, no built-in voice's name, and nothing shaped like a voice id. |

**Responses:**

- `201`: Added.
- `400`: Invalid: `validation_failed`, `invalid_voice_name`, `consent_required`, `unsupported_audio_format`, `audio_too_short`, `audio_too_long` or `audio_silent`.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: `voice_name_taken` or `voice_limit_reached`.
- `413`: Over the upload limit (`audio_too_large`).
- `415`: Not multipart form data (`unsupported_media_type`).
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/voices/{voice_id}`

One voice.

**Needs:** `models:read`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `voice_id` | path | string | yes | `voice-` and 12 hex digits. |

**Responses:**

- `200`: The voice.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such voice (`voice_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `PATCH /_localbase/voices/{voice_id}`

Rename a voice.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `voice_id` | path | string | yes | `voice-` and 12 hex digits. |

**Request:** `application/json`. At most 1024 bytes.

| Field | Type | Required | About |
|---|---|---|---|
| `name` | string | yes | Normalized and trimmed. No control characters, no built-in voice's name, and nothing shaped like a voice id. |

**Responses:**

- `200`: Renamed.
- `400`: Invalid: `validation_failed` or `invalid_voice_name`.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such voice (`voice_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `409`: Another voice uses that name (`voice_name_taken`).
- `413`: The body is over this route's limit.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `DELETE /_localbase/voices/{voice_id}`

Delete a voice and its recording.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `voice_id` | path | string | yes | `voice-` and 12 hex digits. |

**Responses:**

- `200`: Deleted.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such voice (`voice_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

### `GET /_localbase/voices/{voice_id}/audio`

A voice's normalized reference recording.

A recording of a person, so only managers can replay it. A file that no longer matches its checksum answers 404.

**Needs:** `models:manage`.

**Parameters:**

| Name | In | Type | Required | About |
|---|---|---|---|---|
| `voice_id` | path | string | yes | `voice-` and 12 hex digits. |

**Responses:**

- `200`: The WAV.
- `401`: The key is missing or not accepted (`invalid_api_key`). A request carrying `Origin` or `Sec-Fetch-Site` never authenticates with a key.
- `403`: The key lacks the permission this route needs (`insufficient_permissions`).
- `404`: No such voice (`voice_not_found`).
- `405`: Wrong method (`method_not_allowed`); see the `Allow` header.
- `501`: This modality is not enabled on this gateway (`route_disabled`).
- `503`: Try again: memory pressure (`insufficient_memory`), a restarting backend (`service_unavailable`), a model being unloaded (`model_unloading`), a failed start (`backend_startup_failed`), a backend that placed its work differently from the plan (`placement_mismatch`).

## Error codes

Every code this gateway's own routes answer with. `error.code` can also hold a code a backend sent, or null. A later version can add codes, so handle one you don't know by its HTTP status.

- `audio_silent`
- `audio_too_large`
- `audio_too_long`
- `audio_too_short`
- `backend_not_selected`
- `backend_startup_failed`
- `backend_unavailable`
- `catalog_conflict`
- `conflict`
- `consent_required`
- `context_length_exceeded`
- `custom_source_unconfirmed`
- `email_delivery_failed`
- `email_delivery_in_use`
- `email_delivery_not_configured`
- `exceeds_caller_expiry`
- `exceeds_caller_permissions`
- `experimental_model_not_allowed`
- `hf_token_invalid`
- `hf_token_missing`
- `hub_auth_required`
- `hub_rate_limited`
- `hub_rejected`
- `hub_unavailable`
- `import_choose_file`
- `import_not_found`
- `import_task_required`
- `import_unresolved`
- `inference_queue_full`
- `inference_queue_timeout`
- `insufficient_memory`
- `insufficient_permissions`
- `insufficient_storage`
- `invalid_api_key`
- `invalid_request`
- `invalid_voice_name`
- `issuer_required`
- `issuer_unknown`
- `key_not_found`
- `managed_user_exists`
- `managed_user_not_found`
- `method_not_allowed`
- `model_assets_missing`
- `model_busy`
- `model_hardware_ineligible`
- `model_install_consent_required`
- `model_loading`
- `model_not_found`
- `model_not_loaded`
- `model_too_large`
- `model_unloading`
- `no_auth_request_refused`
- `no_recipe`
- `payload_too_large`
- `placement_does_not_fit`
- `placement_mismatch`
- `placement_unsupported`
- `policy_conflict`
- `policy_not_configured`
- `policy_revision_conflict`
- `provider_not_configured`
- `registration_not_found`
- `request_aborted`
- `role_not_found`
- `route_disabled`
- `run_not_found`
- `run_result_unavailable`
- `service_unavailable`
- `storage_unavailable`
- `structured_output_validation_failed`
- `too_many_runs`
- `ui_access_denied`
- `unsafe_path`
- `unsupported_audio_format`
- `unsupported_media_type`
- `unsupported_model_capability`
- `upstream_error`
- `validation_failed`
- `video_generation_failed`
- `video_job_busy`
- `video_job_not_found`
- `voice_limit_reached`
- `voice_name_taken`
- `voice_not_found`
- `voice_not_supported`
- `would_lock_out_admins`
