import {
  createOtelRuntime,
  serverSpanOptions,
} from "../domains/observability/otel";
import { createLogEvent } from "../domains/observability/logging";

const endpoint = process.argv[2];
if (!endpoint) throw new Error("Collector endpoint is required.");

const runtime = createOtelRuntime({
  enabled: true,
  tracesEndpoint: `${endpoint}/v1/traces`,
  logsEndpoint: `${endpoint}/v1/logs`,
  headers: {},
  tracesHeaders: {},
  logsHeaders: {},
  sampleRatio: 1,
  sampler: "always_on",
  source: "persistent",
  displayEndpoint: endpoint,
});
const event = createLogEvent({
  severity: "info",
  eventName: "observability.test",
  category: "logging",
  component: "otel",
  runtime: "gateway",
  message: "Hung collector process-liveness probe.",
});

for (let index = 0; index < 5_000; index++) {
  runtime.emit(event);
  await runtime.withSpan(
    "shutdown.saturation",
    serverSpanOptions("GET", "/health"),
    () => {},
  );
}

const shutdownStartedAt = Date.now();
await runtime.shutdown();
const shutdownReturnedAt = Date.now();
process.stdout.write(
  `${JSON.stringify({
    shutdownMs: shutdownReturnedAt - shutdownStartedAt,
    shutdownReturnedAt,
  })}\n`,
);
