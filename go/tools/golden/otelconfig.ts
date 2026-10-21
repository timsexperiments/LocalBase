// Golden OTLP configuration resolution from the TypeScript code.
//
//   bun run go/tools/golden/otelconfig.ts > go/internal/obs/testdata/otelconfig.json
//
// Saved settings and OTEL_* environments: the effective configuration, or
// the error (field and message) that rejects it.
import { z } from "zod";
import { resolveOtelConfiguration } from "../../../src/domains/observability/otel";
import { formatZodError } from "../../../src/domains/app/commands/errors";

const saved = [
  { otelEndpoint: "", otelHeaders: "", otelSampleRatio: 100 },
  { otelEndpoint: "http://collector:4318", otelHeaders: "x-token=abc", otelSampleRatio: 25 },
  { otelEndpoint: "https://otel.example.com/base/", otelHeaders: "a=1,B=two%20words", otelSampleRatio: 0 },
];
const envs: Record<string, string>[] = [
  {},
  { OTEL_EXPORTER_OTLP_ENDPOINT: "http://env:4318" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "http://env:4318/prefix//" },
  { OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: "https://traces.example/v1/traces" },
  { OTEL_EXPORTER_OTLP_LOGS_ENDPOINT: "https://logs.example:8443/custom" },
  { OTEL_EXPORTER_OTLP_HEADERS: "authorization=Bearer%20x" },
  { OTEL_EXPORTER_OTLP_TRACES_HEADERS: "x-a=1", OTEL_EXPORTER_OTLP_LOGS_HEADERS: "x-b=2" },
  { OTEL_TRACES_SAMPLER: "always_on" },
  { OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "0.1" },
  { OTEL_TRACES_SAMPLER: "parentbased_traceidratio", OTEL_TRACES_SAMPLER_ARG: "1" },
  { OTEL_TRACES_SAMPLER_ARG: "0.3" },
  { OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "2" },
  { OTEL_TRACES_SAMPLER: "bogus" },
  { OTEL_EXPORTER_OTLP_PROTOCOL: "http/protobuf" },
  { OTEL_EXPORTER_OTLP_PROTOCOL: "grpc" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "ftp://x" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "http://u:p@x" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "http://x/?q=1" },
  { OTEL_EXPORTER_OTLP_HEADERS: "content-type=x" },
  { OTEL_EXPORTER_OTLP_HEADERS: "novalue" },
  { OTEL_EXPORTER_OTLP_HEADERS: "a=1,A=2" },
  { OTEL_EXPORTER_OTLP_HEADERS: "a=%zz" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "" },
  { OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "abc" },
  { OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: "-0.5" },
  { OTEL_TRACES_SAMPLER: "traceidratio", OTEL_TRACES_SAMPLER_ARG: " " },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "not a url", OTEL_EXPORTER_OTLP_HEADERS: "bad" },
  { OTEL_TRACES_SAMPLER: "bogus", OTEL_TRACES_SAMPLER_ARG: "0.5" },
  { OTEL_EXPORTER_OTLP_LOGS_PROTOCOL: "http/json", OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: "ftp://t" },
  { OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector.example:80/x" },
];
const cases = [];
for (const config of saved) {
  for (const env of envs) {
    try {
      const c = resolveOtelConfiguration(config, env);
      cases.push({ config, env, result: { ...c, tracesEndpoint: c.tracesEndpoint ?? null, logsEndpoint: c.logsEndpoint ?? null } });
    } catch (error) {
      cases.push({ config, env, error: error instanceof z.ZodError ? formatZodError(error) : (error as Error).message });
    }
  }
}
console.log(JSON.stringify(cases, null, 1));
