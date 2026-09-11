# Operating LocalBase

## Inference queues

LocalBase owns one FIFO queue for each inference type: LLM, speech-to-text, and image generation. Each queue accepts 16 waiting requests by default and gives each request 60 seconds to reach dispatch. Active requests do not count toward the waiting limit.

Foreground `serve` processes can change these limits:

```bash
local-base serve \
  --inference-queue-capacity 32 \
  --inference-queue-timeout-ms 90000
```

The limits apply independently to each inference type. LLM concurrency comes from the running backend's resolved slot count. Speech-to-text and image generation run one request at a time.

Requests keep FIFO order across model IDs. When the head request needs another model, LocalBase waits for active requests on the current model to finish before switching. Later requests cannot bypass that switch, even if they use the current model. A cold LLM starts with one admission slot. Once the backend is ready and its resolved slot count is available, LocalBase admits queued requests for the same model without waiting for the first response to finish.

If the queue is full, the gateway returns HTTP `429` with OpenAI error code `inference_queue_full`. If a request waits past the queue deadline, it returns `429` with code `inference_queue_timeout`. Both responses include `Retry-After: 1`.

The queue deadline covers FIFO, model-transition, and dispatch-owner waiting. It stops when runtime dispatch begins. It does not limit backend startup or response generation. Callers should use their own request deadline for those phases. Aborting the request signal, such as a fetch `AbortController`, removes queued work, prevents cancelled dispatch from starting backend work, or cancels an active response. Cancelling only a local response stream reader is not a substitute for aborting the HTTP request. Before response headers are committed, LocalBase can represent request cancellation as HTTP `499`. After streaming starts, cancellation terminates the stream.

Shutdown, runtime disablement, and memory emergencies reject queued and dispatching work. They do not launch that work later.

## Streams and telemetry

For streaming responses, LocalBase holds the active runtime lease until the response body completes, fails, or the request signal is aborted. Model changes therefore wait for open streams to settle.

For chat completions, the `inference.completed` JSONL event includes `total_duration_ms`, `outcome`, and `queue_wait_ms` when the request used the queue. Sampled chat spans record the same queue time as `localbase.inference.queue.duration_ms` and total time as `localbase.inference.total.duration_ms`. Validated chat events may add token counts, backend prompt and generation timing, and finish reasons. These inference completion fields are not emitted for speech-to-text, image, or embedding requests.

Inference telemetry does not record prompts, generated content, credentials, baggage, or arbitrary request headers. See [OpenTelemetry export](../README.md#opentelemetry-export) for transport and sampling settings.

## Model metadata

Authenticated `GET /_localbase/models` and `GET /_localbase/models/:modelId` report catalog identity, artifact checksums and sizes, catalog memory estimates, selection and installation state, and the observed applied runtime. Metadata reads do not start runtimes or hash model files.

`device.runtime.effectiveSlots` is the backend's resolved launch-plan count. `device.runtime.queueDepth` is the number of waiting requests. `device.runtime.availableCapacity` is the remaining bounded waiting capacity, or `0` when the queue is closed. Queue values are `null` when no authoritative queue snapshot exists. Catalog capabilities, context windows, and output limits remain `null` when LocalBase has no authoritative value.

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
