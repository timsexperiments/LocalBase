import { z } from "zod";

export const otelEndpointSchema = z
  .url()
  .max(2_048)
  .superRefine((value, ctx) => {
    const url = new URL(value);
    if (url.protocol !== "http:" && url.protocol !== "https:") {
      ctx.addIssue({ code: "custom", message: "must use http or https" });
    }
    if (url.username || url.password || url.search || url.hash) {
      ctx.addIssue({
        code: "custom",
        message:
          "must not contain credentials, query parameters, or a fragment; use OTLP headers for authentication",
      });
    }
  });

const headerNameSchema = z
  .string()
  .min(1)
  .max(128)
  .regex(/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/);

export function parseOtelHeaders(
  value: string | undefined,
): Record<string, string> {
  if (!value?.trim()) return {};
  const entries = value.split(",").map((entry) => {
    const separator = entry.indexOf("=");
    if (separator < 1) throw new Error("OTLP headers must use key=value.");
    const name = headerNameSchema.parse(entry.slice(0, separator).trim());
    const rawValue = entry.slice(separator + 1).trim();
    let decoded: string;
    try {
      decoded = decodeURIComponent(rawValue);
    } catch {
      throw new Error(`OTLP header ${name} is not valid percent-encoding.`);
    }
    if (!decoded || decoded.length > 4_096 || /[\r\n]/.test(decoded)) {
      throw new Error(`OTLP header ${name} has an invalid value.`);
    }
    return [name, decoded] as const;
  });
  return Object.fromEntries(entries);
}

export const otelHeadersTextSchema = z
  .string()
  .max(8_192)
  .superRefine((value, ctx) => {
    try {
      parseOtelHeaders(value);
    } catch (error) {
      ctx.addIssue({
        code: "custom",
        message: error instanceof Error ? error.message : String(error),
      });
    }
  });

export const otelSamplerKindSchema = z.enum([
  "always_on",
  "always_off",
  "traceidratio",
  "parentbased_always_on",
  "parentbased_always_off",
  "parentbased_traceidratio",
]);
export type OtelSamplerKind = z.infer<typeof otelSamplerKindSchema>;

const ratioSchema = z.coerce.number().finite().min(0).max(1);
const protocolSchema = z.literal("http/protobuf");

export const otelEnvironmentSchema = z
  .object({
    OTEL_EXPORTER_OTLP_ENDPOINT: otelEndpointSchema.optional(),
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: otelEndpointSchema.optional(),
    OTEL_EXPORTER_OTLP_LOGS_ENDPOINT: otelEndpointSchema.optional(),
    OTEL_EXPORTER_OTLP_HEADERS: otelHeadersTextSchema.optional(),
    OTEL_EXPORTER_OTLP_TRACES_HEADERS: otelHeadersTextSchema.optional(),
    OTEL_EXPORTER_OTLP_LOGS_HEADERS: otelHeadersTextSchema.optional(),
    OTEL_EXPORTER_OTLP_PROTOCOL: protocolSchema.optional(),
    OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: protocolSchema.optional(),
    OTEL_EXPORTER_OTLP_LOGS_PROTOCOL: protocolSchema.optional(),
    OTEL_TRACES_SAMPLER: otelSamplerKindSchema.optional(),
    OTEL_TRACES_SAMPLER_ARG: ratioSchema.optional(),
  })
  .strict()
  .superRefine((value, ctx) => {
    if (
      value.OTEL_TRACES_SAMPLER_ARG !== undefined &&
      value.OTEL_TRACES_SAMPLER !== "traceidratio" &&
      value.OTEL_TRACES_SAMPLER !== "parentbased_traceidratio"
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["OTEL_TRACES_SAMPLER_ARG"],
        message: "is only valid with traceidratio or parentbased_traceidratio",
      });
    }
  });

export type OtelEnvironment = z.infer<typeof otelEnvironmentSchema>;

export const OTEL_ENVIRONMENT_NAMES = [
  "OTEL_EXPORTER_OTLP_ENDPOINT",
  "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
  "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
  "OTEL_EXPORTER_OTLP_HEADERS",
  "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
  "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
  "OTEL_EXPORTER_OTLP_PROTOCOL",
  "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
  "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
  "OTEL_TRACES_SAMPLER",
  "OTEL_TRACES_SAMPLER_ARG",
] as const;

export function parseOtelEnvironment(
  environment: Record<string, string | undefined>,
): OtelEnvironment {
  return otelEnvironmentSchema.parse(
    Object.fromEntries(
      OTEL_ENVIRONMENT_NAMES.flatMap((name) =>
        environment[name] ? [[name, environment[name]]] : [],
      ),
    ),
  );
}

export function otelServiceEnvironment(
  environment: Record<string, string | undefined>,
): Record<string, string> {
  const parsed = parseOtelEnvironment(environment);
  return Object.fromEntries(
    Object.entries(parsed).map(([name, value]) => [name, String(value)]),
  );
}

export const otelServiceEnvironmentSchema = z
  .record(z.string(), z.string())
  .superRefine((value, ctx) => {
    const unexpected = Object.keys(value).filter(
      (name) =>
        !OTEL_ENVIRONMENT_NAMES.includes(
          name as (typeof OTEL_ENVIRONMENT_NAMES)[number],
        ),
    );
    if (unexpected.length > 0) {
      ctx.addIssue({
        code: "custom",
        message: `unsupported OTEL environment variable: ${unexpected[0]}`,
      });
      return;
    }
    const parsed = otelEnvironmentSchema.safeParse(value);
    if (!parsed.success) {
      for (const issue of parsed.error.issues) {
        ctx.addIssue({
          code: "custom",
          path: issue.path,
          message: issue.message,
        });
      }
    }
  });

export function sanitizedOtelEndpoint(value: string | undefined): string {
  if (!value) return "";
  const parsed = otelEndpointSchema.safeParse(value);
  if (!parsed.success) return "";
  const url = new URL(parsed.data);
  return `${url.protocol}//${url.host}${url.pathname}`;
}
