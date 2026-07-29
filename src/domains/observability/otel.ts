import {
  DiagLogLevel,
  INVALID_SPAN_CONTEXT,
  SpanKind,
  SpanStatusCode,
  context,
  diag,
  propagation,
  trace,
  type Attributes,
  type Context,
  type Span,
  type SpanOptions,
} from "@opentelemetry/api";
import { logs, SeverityNumber } from "@opentelemetry/api-logs";
import { AsyncLocalStorageContextManager } from "@opentelemetry/context-async-hooks";
import {
  W3CTraceContextPropagator,
  ExportResultCode,
  type ExportResult,
} from "@opentelemetry/core";
import { OTLPLogExporter } from "@opentelemetry/exporter-logs-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import { resourceFromAttributes } from "@opentelemetry/resources";
import {
  BatchLogRecordProcessor,
  LoggerProvider,
} from "@opentelemetry/sdk-logs";
import type {
  LogRecordExporter,
  ReadableLogRecord,
} from "@opentelemetry/sdk-logs";
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  BasicTracerProvider,
  BatchSpanProcessor,
  ParentBasedSampler,
  TraceIdRatioBasedSampler,
  type ReadableSpan,
  type SpanExporter,
} from "@opentelemetry/sdk-trace-base";
import {
  ATTR_SERVICE_NAME,
  ATTR_SERVICE_NAMESPACE,
  ATTR_SERVICE_VERSION,
} from "@opentelemetry/semantic-conventions";
import type { LocalBaseConfig } from "../../manager";
import { redactExternalLogText, type LogEvent } from "./logging";
import {
  otelEndpointSchema,
  parseOtelEnvironment,
  parseOtelHeaders,
  sanitizedOtelEndpoint,
  type OtelSamplerKind,
} from "./otel-config";

export const OTEL_SERVICE_NAME = "local-base";
export const OTEL_SERVICE_NAMESPACE = "localbase";
export const OTEL_SERVICE_VERSION = "0.1.0";
export const OTEL_MAX_QUEUE_SIZE = 2_048;
export const OTEL_MAX_BATCH_SIZE = 256;
export const OTEL_EXPORT_DELAY_MS = 1_000;
export const OTEL_EXPORT_TIMEOUT_MS = 5_000;

export type OtelConfiguration = {
  enabled: boolean;
  tracesEndpoint?: string;
  logsEndpoint?: string;
  headers: Record<string, string>;
  tracesHeaders: Record<string, string>;
  logsHeaders: Record<string, string>;
  sampleRatio: number;
  sampler: OtelSamplerKind;
  source: "environment" | "persistent";
  displayEndpoint: string;
};

function signalEndpoint(base: string, signal: "traces" | "logs"): string {
  const url = new URL(base);
  const prefix = url.pathname.replace(/\/+$/, "");
  url.pathname = `${prefix}/v1/${signal}`;
  return url.toString();
}

export function resolveOtelConfiguration(
  config: Pick<
    LocalBaseConfig,
    "otelEndpoint" | "otelHeaders" | "otelSampleRatio"
  >,
  environment: Record<string, string | undefined> = process.env,
): OtelConfiguration {
  const parsed = parseOtelEnvironment(environment);
  const persistentEndpoint = config.otelEndpoint
    ? otelEndpointSchema.parse(config.otelEndpoint)
    : undefined;
  const base = parsed.OTEL_EXPORTER_OTLP_ENDPOINT ?? persistentEndpoint;
  const tracesEndpoint =
    parsed.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ??
    (base ? signalEndpoint(base, "traces") : undefined);
  const logsEndpoint =
    parsed.OTEL_EXPORTER_OTLP_LOGS_ENDPOINT ??
    (base ? signalEndpoint(base, "logs") : undefined);
  const sampler = parsed.OTEL_TRACES_SAMPLER ?? "parentbased_traceidratio";
  const sampleRatio =
    parsed.OTEL_TRACES_SAMPLER_ARG ??
    (parsed.OTEL_TRACES_SAMPLER ? 1 : config.otelSampleRatio / 100);
  const headers = parseOtelHeaders(
    parsed.OTEL_EXPORTER_OTLP_HEADERS ?? config.otelHeaders,
  );
  const environmentEndpoint =
    parsed.OTEL_EXPORTER_OTLP_ENDPOINT ??
    parsed.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ??
    parsed.OTEL_EXPORTER_OTLP_LOGS_ENDPOINT;
  const hasEnvironmentOverride = Object.keys(parsed).length > 0;
  return {
    enabled: Boolean(tracesEndpoint || logsEndpoint),
    tracesEndpoint,
    logsEndpoint,
    headers,
    tracesHeaders: parseOtelHeaders(parsed.OTEL_EXPORTER_OTLP_TRACES_HEADERS),
    logsHeaders: parseOtelHeaders(parsed.OTEL_EXPORTER_OTLP_LOGS_HEADERS),
    sampleRatio,
    sampler,
    source: hasEnvironmentOverride ? "environment" : "persistent",
    displayEndpoint: sanitizedOtelEndpoint(
      environmentEndpoint ?? persistentEndpoint,
    ),
  };
}

export type OtelDiagnostic = (
  severity: "warn" | "error",
  message: string,
  attributes?: Record<string, unknown>,
) => void;

class MonitoredExporter<T> {
  private lastFailureAt = 0;

  constructor(
    private readonly delegate: {
      export(items: T[], callback: (result: ExportResult) => void): void;
      shutdown(): Promise<void>;
      forceFlush?(): Promise<void>;
    },
    private readonly signal: "logs" | "traces",
    private readonly diagnostic: OtelDiagnostic,
  ) {}

  export(items: T[], callback: (result: ExportResult) => void): void {
    try {
      this.delegate.export(items, (result) => {
        if (result.code !== ExportResultCode.SUCCESS) {
          const now = Date.now();
          if (now - this.lastFailureAt >= 60_000) {
            this.lastFailureAt = now;
            this.diagnostic("warn", "OTLP export failed.", {
              signal: this.signal,
              batchSize: items.length,
            });
          }
        }
        callback(result);
      });
    } catch {
      this.diagnostic("warn", "OTLP exporter rejected a batch.", {
        signal: this.signal,
        batchSize: items.length,
      });
      callback({ code: ExportResultCode.FAILED });
    }
  }

  async forceFlush(): Promise<void> {
    await this.delegate.forceFlush?.();
  }

  async shutdown(): Promise<void> {
    await this.delegate.shutdown();
  }
}

export interface OtelRuntime {
  readonly enabled: boolean;
  emit(event: LogEvent): void;
  extract(headers: Headers): Context;
  inject(headers: Headers, activeContext?: Context): void;
  withSpan<T>(
    name: string,
    options: SpanOptions,
    operation: (span: Span) => Promise<T> | T,
    parent?: Context,
  ): Promise<T>;
  activeCorrelation(): { traceId: string; spanId: string } | undefined;
  forceFlush(): Promise<void>;
  shutdown(): Promise<void>;
}

class NoopOtelRuntime implements OtelRuntime {
  readonly enabled = false;
  emit(): void {}
  extract(): Context {
    return context.active();
  }
  inject(): void {}
  async withSpan<T>(
    _name: string,
    _options: SpanOptions,
    operation: (span: Span) => Promise<T> | T,
  ): Promise<T> {
    return await operation(trace.wrapSpanContext(INVALID_SPAN_CONTEXT));
  }
  activeCorrelation(): undefined {
    return undefined;
  }
  async forceFlush(): Promise<void> {}
  async shutdown(): Promise<void> {}
}

const severityNumbers: Record<LogEvent["severity"], SeverityNumber> = {
  debug: SeverityNumber.DEBUG,
  info: SeverityNumber.INFO,
  warn: SeverityNumber.WARN,
  error: SeverityNumber.ERROR,
};

function eventAttributes(event: LogEvent): Attributes {
  return {
    "localbase.event_id": event.id,
    "localbase.schema_version": event.schemaVersion,
    "event.name": event.eventName,
    "event.category": event.category,
    "localbase.component": event.component,
    "localbase.runtime": event.runtime,
    ...(event.requestId ? { "localbase.request_id": event.requestId } : {}),
    ...(event.http
      ? {
          "http.request.method": event.http.method,
          "http.route": event.http.path,
          "http.response.status_code": event.http.status,
          "localbase.http.duration_ms": event.http.durationMs,
        }
      : {}),
    ...(event.error
      ? {
          "error.type": event.error.type,
          "error.message": event.error.message,
          ...(event.error.code ? { "error.code": event.error.code } : {}),
        }
      : {}),
    ...event.attributes,
  };
}

function samplerFor(configuration: OtelConfiguration) {
  switch (configuration.sampler) {
    case "always_on":
      return new AlwaysOnSampler();
    case "always_off":
      return new AlwaysOffSampler();
    case "traceidratio":
      return new TraceIdRatioBasedSampler(configuration.sampleRatio);
    case "parentbased_always_on":
      return new ParentBasedSampler({ root: new AlwaysOnSampler() });
    case "parentbased_always_off":
      return new ParentBasedSampler({ root: new AlwaysOffSampler() });
    case "parentbased_traceidratio":
      return new ParentBasedSampler({
        root: new TraceIdRatioBasedSampler(configuration.sampleRatio),
      });
  }
}

type SanitizedTraceError = {
  name: string;
  message: string;
  stack?: string;
  code?: string;
};

/** Trace payloads use the same bounded redaction boundary as local logs. */
export function sanitizeTraceError(error: unknown): SanitizedTraceError {
  const source =
    error && typeof error === "object"
      ? (error as Record<string, unknown>)
      : undefined;
  const name = redactExternalLogText(
    source?.name ?? (error instanceof Error ? error.name : "Error"),
    128,
  );
  const message = redactExternalLogText(
    source?.message ?? (error instanceof Error ? error.message : error),
    2_048,
  );
  const stack =
    source?.stack === undefined
      ? undefined
      : redactExternalLogText(source.stack, 4_096);
  const code =
    source?.code === undefined
      ? undefined
      : redactExternalLogText(source.code, 128);
  return {
    name: name || "Error",
    message: message || "Unknown error",
    ...(stack ? { stack } : {}),
    ...(code ? { code } : {}),
  };
}

class ActiveOtelRuntime implements OtelRuntime {
  readonly enabled = true;
  private readonly tracerProvider: BasicTracerProvider;
  private readonly loggerProvider?: LoggerProvider;
  private readonly otelLogger?: ReturnType<LoggerProvider["getLogger"]>;
  private readonly tracer;
  private readonly contextManager = new AsyncLocalStorageContextManager();
  private closed = false;

  constructor(configuration: OtelConfiguration, diagnostic: OtelDiagnostic) {
    const diagnosticTimes = new Map<string, number>();
    const reportSdkDiagnostic = (
      severity: "warn" | "error",
      values: unknown[],
    ) => {
      const detail = values
        .map((value) =>
          value instanceof Error ? value.message : String(value),
        )
        .join(" ")
        .slice(0, 1_024);
      const key = severity;
      const now = Date.now();
      if (now - (diagnosticTimes.get(key) ?? 0) < 60_000) return;
      diagnosticTimes.set(key, now);
      diagnostic(severity, "OpenTelemetry SDK diagnostic.", {
        signal: "sdk",
        detail,
      });
    };
    diag.setLogger(
      {
        error: (...values) => reportSdkDiagnostic("error", values),
        warn: (...values) => reportSdkDiagnostic("warn", values),
        info() {},
        debug() {},
        verbose() {},
      },
      DiagLogLevel.WARN,
    );
    const resource = resourceFromAttributes({
      [ATTR_SERVICE_NAME]: OTEL_SERVICE_NAME,
      [ATTR_SERVICE_NAMESPACE]: OTEL_SERVICE_NAMESPACE,
      [ATTR_SERVICE_VERSION]: OTEL_SERVICE_VERSION,
    });
    const traceProcessors = configuration.tracesEndpoint
      ? [
          new BatchSpanProcessor(
            new MonitoredExporter<ReadableSpan>(
              new OTLPTraceExporter({
                url: configuration.tracesEndpoint,
                headers: {
                  ...configuration.headers,
                  ...configuration.tracesHeaders,
                },
                timeoutMillis: OTEL_EXPORT_TIMEOUT_MS,
              }),
              "traces",
              diagnostic,
            ) satisfies SpanExporter,
            {
              maxQueueSize: OTEL_MAX_QUEUE_SIZE,
              maxExportBatchSize: OTEL_MAX_BATCH_SIZE,
              scheduledDelayMillis: OTEL_EXPORT_DELAY_MS,
              exportTimeoutMillis: OTEL_EXPORT_TIMEOUT_MS,
            },
          ),
        ]
      : [];
    this.tracerProvider = new BasicTracerProvider({
      resource,
      sampler: samplerFor(configuration),
      spanProcessors: traceProcessors,
    });
    this.contextManager.enable();
    context.setGlobalContextManager(this.contextManager);
    trace.setGlobalTracerProvider(this.tracerProvider);
    propagation.setGlobalPropagator(new W3CTraceContextPropagator());
    this.tracer = this.tracerProvider.getTracer(
      OTEL_SERVICE_NAME,
      OTEL_SERVICE_VERSION,
    );

    if (configuration.logsEndpoint) {
      this.loggerProvider = new LoggerProvider({
        resource,
        processors: [
          new BatchLogRecordProcessor({
            exporter: new MonitoredExporter<ReadableLogRecord>(
              new OTLPLogExporter({
                url: configuration.logsEndpoint,
                headers: {
                  ...configuration.headers,
                  ...configuration.logsHeaders,
                },
                timeoutMillis: OTEL_EXPORT_TIMEOUT_MS,
              }),
              "logs",
              diagnostic,
            ) satisfies LogRecordExporter,
            maxQueueSize: OTEL_MAX_QUEUE_SIZE,
            maxExportBatchSize: OTEL_MAX_BATCH_SIZE,
            scheduledDelayMillis: OTEL_EXPORT_DELAY_MS,
            exportTimeoutMillis: OTEL_EXPORT_TIMEOUT_MS,
          }),
        ],
      });
      logs.setGlobalLoggerProvider(this.loggerProvider);
      this.otelLogger = this.loggerProvider.getLogger(
        OTEL_SERVICE_NAME,
        OTEL_SERVICE_VERSION,
      );
    }
  }

  emit(event: LogEvent): void {
    if (this.closed) return;
    try {
      this.otelLogger?.emit({
        timestamp: Date.parse(event.timestamp),
        severityNumber: severityNumbers[event.severity],
        severityText: event.severity.toUpperCase(),
        body: event.message,
        attributes: eventAttributes(event),
      });
    } catch {
      // Local JSONL remains authoritative when the secondary sink fails.
    }
  }

  extract(headers: Headers): Context {
    return propagation.extract(context.active(), headers, {
      get(carrier, key) {
        return carrier.get(key) ?? undefined;
      },
      keys(carrier) {
        return [...carrier.keys()];
      },
    });
  }

  inject(headers: Headers, activeContext = context.active()): void {
    propagation.inject(activeContext, headers, {
      set(carrier, key, value) {
        carrier.set(key, value);
      },
    });
  }

  async withSpan<T>(
    name: string,
    options: SpanOptions,
    operation: (span: Span) => Promise<T> | T,
    parent = context.active(),
  ): Promise<T> {
    const span = this.tracer.startSpan(name, options, parent);
    const activeContext = trace.setSpan(parent, span);
    try {
      return await context.with(activeContext, operation, undefined, span);
    } catch (error) {
      const sanitized = sanitizeTraceError(error);
      span.recordException(sanitized);
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: sanitized.message,
      });
      throw error;
    } finally {
      span.end();
    }
  }

  activeCorrelation(): { traceId: string; spanId: string } | undefined {
    const activeSpan = trace.getActiveSpan();
    if (!activeSpan?.isRecording()) return undefined;
    const spanContext = activeSpan.spanContext();
    if (!spanContext || !trace.isSpanContextValid(spanContext))
      return undefined;
    return { traceId: spanContext.traceId, spanId: spanContext.spanId };
  }

  async forceFlush(): Promise<void> {
    await Promise.allSettled([
      this.tracerProvider.forceFlush(),
      this.loggerProvider?.forceFlush(),
    ]);
  }

  async shutdown(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    await this.forceFlush();
    await Promise.allSettled([
      this.tracerProvider.shutdown(),
      this.loggerProvider?.shutdown(),
    ]);
    // The process owns one runtime. Leaving the registered context manager
    // enabled avoids invalidating async work that was already handed to Bun.
  }
}

export function createOtelRuntime(
  configuration: OtelConfiguration,
  diagnostic: OtelDiagnostic = () => {},
): OtelRuntime {
  return configuration.enabled
    ? new ActiveOtelRuntime(configuration, diagnostic)
    : new NoopOtelRuntime();
}

const knownHttpRoutes = new Set([
  "/health",
  "/v1/models",
  "/v1/chat/completions",
  "/v1/embeddings",
  "/v1/audio/transcriptions",
  "/v1/audio/translations",
  "/v1/images/generations",
]);

export function normalizedOtelRoute(route: string): string {
  const pathname = route.split("?", 1)[0];
  return knownHttpRoutes.has(pathname) ? pathname : "unmatched-route";
}

export function serverSpanName(method: string, route: string): string {
  return `${method.toUpperCase()} ${normalizedOtelRoute(route)}`;
}

export function serverSpanOptions(method: string, route: string): SpanOptions {
  return {
    kind: SpanKind.SERVER,
    attributes: {
      "http.request.method": method,
      "http.route": normalizedOtelRoute(route),
    },
  };
}

export function internalSpanOptions(attributes?: Attributes): SpanOptions {
  return { kind: SpanKind.INTERNAL, attributes };
}

export function clientSpanOptions(attributes?: Attributes): SpanOptions {
  return { kind: SpanKind.CLIENT, attributes };
}
