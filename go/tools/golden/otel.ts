// Golden OTLP export rules from the TypeScript code and its SDK.
//
//   bun run go/tools/golden/otel.ts > go/internal/otlp/testdata/golden.json
//
// The ratio sampler's decisions, Retry-After handling, and the export
// failure reporter over a scripted outage (fake clock).
import { TraceIdRatioBasedSampler } from "@opentelemetry/sdk-trace-base";
import { ROOT_CONTEXT, SpanKind } from "@opentelemetry/api";
import { createHash } from "node:crypto";
import { retryDelayMilliseconds } from "../../../src/domains/observability/otel";
import { ExportFailureReporter } from "../../../src/domains/observability/export-failures";

const ids: string[] = [];
for (let i = 0; i < 400; i++) ids.push(createHash("sha256").update(String(i)).digest("hex").slice(0, 32));
ids.push("ffffffffffffffffffffffffffffffff", "00000000000000000000000000000001", "0af7651916cd43dd8448eb211c80319c");
const sampling = [0, 0.01, 0.25, 0.5, 0.999, 1].map((ratio) => {
  const s = new TraceIdRatioBasedSampler(ratio);
  return { ratio, sampled: ids.map((id) => s.shouldSample(ROOT_CONTEXT, id, "x", SpanKind.SERVER, {}, []).decision === 2) };
});

const now = Date.parse("2026-10-20T10:00:00Z");
const deadline = now + 5000;
const retry = [null, "0", "1", "4", "5", "6", "-1", "abc", "1.5", new Date(now + 2000).toUTCString(),
  new Date(now - 2000).toUTCString(), new Date(now + 9000).toUTCString(), "99999999999999999999"].map((value) => ({
  value, fallback: 250, delay: retryDelayMilliseconds(value, now, deadline, 250) ?? null,
}));

// The reporter: failures at t (ms), recoveries, a flush.
let clock = 0;
const events: unknown[] = [];
const reporter = new ExportFailureReporter((severity, message, attributes) => events.push({ at: clock, severity, message, attributes }), () => clock, 1000);
const script: [number, string, string?][] = [
  [0, "fail:traces:collector responded with HTTP 503"], [100, "fail:traces:collector responded with HTTP 503"],
  [200, "fail:traces:collector unreachable"], [1300, "fail:traces:collector unreachable"], [1400, "recover:traces"],
  [1500, "recover:traces"], [1600, "fail:logs:request timed out after 5000 ms"], [1700, "fail:logs:request timed out after 5000 ms"],
  [5000, "fail:logs:collector responded with HTTP 500"], [5100, "fail:traces:x"], [5200, "fail:traces:x"], [5300, "flush"],
];
for (const [at, step] of script) {
  clock = at;
  const [kind, signal, ...rest] = step.split(":");
  if (kind === "fail") {
    reporter.failure(signal!, { signal: signal!, severity: "warn", message: "OTLP export failed.", detail: rest.join(":"), items: 3, attributes: { batchSize: 3 } });
  } else if (kind === "recover") {
    reporter.recovered(signal!);
  } else {
    reporter.flush();
  }
}
console.log(JSON.stringify({ ids, sampling, now, retry, script, events }, null, 1));
