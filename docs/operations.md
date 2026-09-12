# Operating LocalBase

## Inference queues

LocalBase owns one FIFO queue for each inference type: LLM, speech-to-text, text-to-speech, and image generation. Each queue accepts 16 waiting requests by default and gives each request 60 seconds to reach dispatch. Active requests do not count toward the waiting limit.

Foreground `serve` processes can change these limits:

```bash
local-base serve \
  --inference-queue-capacity 32 \
  --inference-queue-timeout-ms 90000
```

The limits apply independently to each inference type. LLM concurrency comes from the running backend's resolved slot count. Speech-to-text, text-to-speech, and image generation run one request at a time.

Requests keep FIFO order across model IDs. When the head request needs another model, LocalBase waits for active requests on the current model to finish before switching. Later requests cannot bypass that switch, even if they use the current model. A cold LLM starts with one admission slot. Once the backend is ready and its resolved slot count is available, LocalBase admits queued requests for the same model without waiting for the first response to finish.

If the queue is full, the gateway returns HTTP `429` with OpenAI error code `inference_queue_full`. If a request waits past the queue deadline, it returns `429` with code `inference_queue_timeout`. Both responses include `Retry-After: 1`.

The queue deadline covers FIFO, model-transition, and dispatch-owner waiting. It stops when runtime dispatch begins. It does not limit backend startup or response generation. Callers should use their own request deadline for those phases. Aborting the request signal, such as a fetch `AbortController`, removes queued work, prevents cancelled dispatch from starting backend work, or cancels an active response. Cancelling only a local response stream reader is not a substitute for aborting the HTTP request. Before response headers are committed, LocalBase can represent request cancellation as HTTP `499`. After streaming starts, cancellation terminates the stream.

Shutdown, runtime disablement, and memory emergencies reject queued and dispatching work. They do not launch that work later.

TTS runs one cold `llama-tts` child per admitted request. Its estimated 8 GiB job demand is reserved until that child exits. Cancellation, timeout, disablement, shutdown, and emergency eviction terminate the child before releasing the reservation or deleting its private prompt and WAV files. A generation that reaches the 256-frame native cap is rejected as potentially truncated. TTS releases its inference admission after native generation and WAV validation; response-body telemetry does not hold that permit while the client downloads the WAV.

## Streams and telemetry

For streaming responses, LocalBase holds the active runtime lease until the response body completes, fails, or the request signal is aborted. Model changes therefore wait for open streams to settle.

Every HTTP span records the `x-localbase-request-id` value as `localbase.request_id`. LocalBase continues valid incoming W3C trace context and forwards it to native HTTP runtimes. The JSONL `http.request` event records the final gateway status and the time until the response body settles.

For admitted chat, embedding, transcription, speech, and image requests, telemetry uses this content-safe mapping:

| Measurement                   | JSONL `inference.completed`                                                                                 | OTLP `localbase.inference`                                                                                                                   | Non-streaming `Server-Timing` |
| ----------------------------- | ----------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| Request and model             | `requestId`, `model_id`, `artifact_revision`, `quantization`, `runtime_name`                                | `localbase.request_id`, `localbase.inference.model_id`, `.artifact.revision`, `.model.quantization`, `.runtime.name`                         | -                             |
| Admission                     | `queue_wait_ms`, `admission_active`, `admission_slots`, `admission_waiting`                                 | `localbase.inference.queue.duration_ms`, `.admission.active`, `.admission.slots`, `.admission.waiting`                                       | `queue`                       |
| Structured-output preparation | `json_schema_preparation_ms`                                                                                | `localbase.inference.json_schema.preparation.duration_ms`                                                                                    | `prepare`                     |
| Backend timings               | `prompt_duration_ms`, `predicted_duration_ms`                                                               | `localbase.inference.backend.prompt.duration_ms`, `.backend.predicted.duration_ms`                                                           | `prompt`, `generation`        |
| Usage                         | `prompt_tokens`, `completion_tokens`, `total_tokens`                                                        | `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `localbase.inference.usage.total_tokens`                                          | -                             |
| Terminal result               | `outcome`, `http_status`, optional `upstream_status`, optional `terminal_source`, optional `finish_reasons` | `localbase.inference.outcome`, optional `.terminal.source`, `http.response.status_code`, optional `.upstream.status_code`, `.finish_reasons` | -                             |
| Total                         | `total_duration_ms`                                                                                         | `localbase.inference.total.duration_ms`                                                                                                      | `localbase`                   |

Each finite backend token or timing field is independent. LocalBase omits missing or invalid fields instead of deriving them. Embeddings report validated prompt and total token counts; native media backends currently expose no validated token or phase metrics. Unknown backend finish-reason strings become `unknown`. Terminal sources are present only when LocalBase directly observes the source, such as request abortion, response cancellation, response-stream failure, response validation, memory admission failure, or the speech deadline.

`Server-Timing` reports only measurements known before response headers. The `localbase` value ends when LocalBase constructs the headers, not when the client finishes downloading the body. Streaming responses omit `Server-Timing`. Queue failures do not create inference spans; they annotate the HTTP span and emit one content-safe `inference.admission-rejected` JSONL event with the known queue error code and source. Only queue-owned timeout errors report queue elapsed time. Schema rejections remain on the HTTP span without an inference event. Shared runtime startup is not reported as per-request model-load time.

For JSON Schema response formats, `inference.completed` adds `json_schema_requested`, `json_schema_native_mode` (`requested` records intent, not proof that generation completed), `json_schema_preparation_ms`, and `json_schema_validation` (`passed`, `failed`, `skipped`, `not_performed`, or `not_performed_streaming`), with bounded `json_schema_skip_reasons` when applicable. Sampled inference spans use the equivalent `localbase.inference.json_schema.*` attributes; invalid or unsupported schemas instead record `localbase.json_schema.preparation.outcome`, `localbase.json_schema.preparation.duration_ms`, and native mode `not_started` on the pre-admission HTTP span without creating an inference span. Telemetry never records the schema, schema name, response content, or validator errors.

Inference telemetry does not record prompts, generated content, credentials, baggage, or arbitrary request headers. See [OpenTelemetry export](../README.md#opentelemetry-export) for transport and sampling settings.

## Model metadata

Authenticated `GET /_localbase/models` and `GET /_localbase/models/:modelId` report catalog identity, artifact checksums and sizes, catalog memory estimates, selection and installation state, and the observed applied runtime. Metadata reads do not start runtimes or hash model files.

`device.runtime.effectiveSlots` is the backend's resolved launch-plan count. `device.runtime.queueDepth` is the number of waiting requests. `device.runtime.availableCapacity` is the remaining bounded waiting capacity, or `0` when the queue is closed. Queue values are `null` when no authoritative queue snapshot exists. Catalog capabilities, context windows, and output limits remain `null` when LocalBase has no authoritative value.

## Managed startup model installation

When managed startup must install a selected model, service status remains `starting`. Run `local-base logs --follow` to see download progress, validation, checksum or cached-identity verification, completion, and terminal failure phases. Progress events report observed bytes and integer percentages, not an estimated completion time.

## Health and readiness

Public `GET` and `HEAD /health/ready` report request admission readiness. HTTP `200` returns `status: "ready"`, `reason: "request_admission_available"`, and the configured inference types that have bounded queue capacity. HTTP `503` returns `status: "unready"`, an empty `modalities` array, and reason `no_request_admission` or `gateway_stopping`.

`/health/ready` does not start runtimes or refresh configuration. Use public `/health` for process liveness. Readiness means at least one configured inference type can enter its queue. It does not promise that a backend is already running or that a response will finish within a caller's deadline.

## Integrated qualification

Pull requests already run the full Bun test suite and a compiled Linux x64 runtime smoke test. On a Linux x64 host or container, reuse the same smoke path for final integrated qualification:

```bash
bun run scripts/release-artifacts.ts build \
  --target linux-x64 \
  --output extracted/linux-x64
chmod +x extracted/linux-x64/local-base-linux-x64
mkdir -p "$RUNNER_TEMP"
LOCALBASE_SMOKE_TARGET=linux-x64 \
LOCALBASE_SMOKE_CLI="$PWD/extracted/linux-x64/local-base-linux-x64" \
bun test --timeout 120000 scripts/runtime-smoke.test.ts
```

Set `RUNNER_TEMP` to a dedicated writable directory before running the commands. Release qualification uses the same `scripts/runtime-smoke.test.ts` entry point for each compiled target, with the target-specific `LOCALBASE_SMOKE_TARGET` and `LOCALBASE_SMOKE_CLI` values.
