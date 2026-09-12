import { SpanStatusCode, type Span } from "@opentelemetry/api";
import { z } from "zod";
import type { InferencePermitSnapshot } from "../runtime/inference-queue";
import type { RuntimeModality } from "../runtime/modality";
import type { ILogger } from "./logging";

type Usage = Readonly<{
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
}>;

type Timings = Readonly<{ promptMs?: number; predictedMs?: number }>;

export type InferenceOutcome = "completed" | "cancelled" | "error";

export type InferenceTerminalSource =
  | "memory_admission"
  | "request_aborted"
  | "response_cancelled"
  | "response_stream_error"
  | "response_validation"
  | "speech_timeout"
  | "upstream_error_event";

export type InferenceTerminal = Readonly<{
  outcome: InferenceOutcome;
  httpStatus: number;
  source?: InferenceTerminalSource;
}>;

export type InferenceMetadata = Readonly<{
  modelId: string;
  modality: RuntimeModality;
  runtimeName: string;
  catalog?: Readonly<{
    artifactRevision: string;
    quantization: string;
  }>;
  admission: InferencePermitSnapshot;
}>;

export type StructuredOutputValidationTelemetry = Readonly<{
  outcome: "passed" | "failed" | "skipped";
  skipReasons: readonly (
    "content_filter" | "non_completed" | "refusal" | "tool_calls" | "truncation"
  )[];
}>;

type StructuredOutputTelemetry = Readonly<{
  preparationDurationMs: number;
  streaming: boolean;
}>;

export function recordStructuredOutputPreparation(
  span: Span | undefined,
  input: Readonly<{
    outcome: "supported" | "invalid" | "unsupported";
    durationMs: number;
    requestId: string;
  }>,
): void {
  if (!span) return;
  span.setAttribute("localbase.request_id", input.requestId);
  span.setAttribute("localbase.json_schema.requested", true);
  span.setAttribute(
    "localbase.json_schema.preparation.duration_ms",
    input.durationMs,
  );
  span.setAttribute("localbase.json_schema.preparation.outcome", input.outcome);
  if (input.outcome !== "supported") {
    span.setAttribute("localbase.json_schema.native_mode", "not_started");
  }
}

const completionMetadataSchema = z
  .object({
    choices: z
      .array(z.object({ finish_reason: z.unknown().optional() }).passthrough())
      .optional(),
    usage: z
      .object({
        prompt_tokens: z.unknown().optional(),
        completion_tokens: z.unknown().optional(),
        total_tokens: z.unknown().optional(),
      })
      .nullable()
      .optional(),
    timings: z
      .object({
        prompt_ms: z.unknown().optional(),
        predicted_ms: z.unknown().optional(),
      })
      .optional(),
  })
  .passthrough();

function finiteNonnegative(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value >= 0
    ? value
    : undefined;
}

const finishReasonSchema = z.enum([
  "stop",
  "length",
  "tool_calls",
  "content_filter",
  "function_call",
]);
type FinishReason = z.infer<typeof finishReasonSchema> | "unknown";

function contentSafeFinishReason(value: unknown): FinishReason | undefined {
  if (typeof value !== "string") return undefined;
  const parsed = finishReasonSchema.safeParse(value);
  return parsed.success ? parsed.data : "unknown";
}

function serverTimingMetric(name: string, durationMs: number): string {
  return `${name};dur=${durationMs.toFixed(2)}`;
}

/** Records only validated, content-free backend completion metadata. */
export class InferenceTelemetry {
  private usage: Usage = {};
  private timings: Timings = {};
  private readonly finishReasons = new Set<FinishReason>();
  private upstreamStatus: number | undefined;
  private structuredOutputValidation:
    | "passed"
    | "failed"
    | "skipped"
    | "not_performed"
    | "not_performed_streaming"
    | undefined;
  private structuredOutputSkipReasons: string | undefined;
  private finished = false;

  constructor(
    private readonly input: Readonly<{
      metadata: InferenceMetadata;
      requestId: string;
      startedAt: number;
      queueWaitMs?: number;
      streaming: boolean;
      structuredOutput?: StructuredOutputTelemetry;
      logger: Pick<ILogger, "event">;
      span: Span;
    }>,
  ) {
    input.span.setAttribute(
      "localbase.inference.model_id",
      input.metadata.modelId,
    );
    input.span.setAttribute(
      "localbase.inference.runtime.name",
      input.metadata.runtimeName,
    );
    if (input.metadata.catalog) {
      input.span.setAttribute(
        "localbase.inference.artifact.revision",
        input.metadata.catalog.artifactRevision,
      );
      input.span.setAttribute(
        "localbase.inference.model.quantization",
        input.metadata.catalog.quantization,
      );
    }
    input.span.setAttribute(
      "localbase.inference.admission.active",
      input.metadata.admission.active,
    );
    input.span.setAttribute(
      "localbase.inference.admission.slots",
      input.metadata.admission.slots,
    );
    input.span.setAttribute(
      "localbase.inference.admission.waiting",
      input.metadata.admission.waiting,
    );
    input.span.setAttribute("localbase.request_id", input.requestId);
    if (input.queueWaitMs !== undefined) {
      input.span.setAttribute(
        "localbase.inference.queue.duration_ms",
        input.queueWaitMs,
      );
    }
    if (input.structuredOutput) {
      this.structuredOutputValidation = input.structuredOutput.streaming
        ? "not_performed_streaming"
        : "not_performed";
      input.span.setAttribute(
        "localbase.inference.json_schema.requested",
        true,
      );
      input.span.setAttribute(
        "localbase.inference.json_schema.native_mode",
        "requested",
      );
      input.span.setAttribute(
        "localbase.inference.json_schema.preparation.duration_ms",
        input.structuredOutput.preparationDurationMs,
      );
    }
  }

  observeValidatedBackendMetadata(value: unknown): void {
    const parsed = completionMetadataSchema.safeParse(value);
    if (!parsed.success) return;
    const event = parsed.data;
    for (const choice of event.choices ?? []) {
      const finishReason = contentSafeFinishReason(choice.finish_reason);
      if (finishReason) this.finishReasons.add(finishReason);
    }
    const promptTokens = finiteNonnegative(event.usage?.prompt_tokens);
    const completionTokens = finiteNonnegative(event.usage?.completion_tokens);
    const totalTokens = finiteNonnegative(event.usage?.total_tokens);
    this.usage = {
      ...this.usage,
      ...(promptTokens === undefined ? {} : { promptTokens }),
      ...(completionTokens === undefined ? {} : { completionTokens }),
      ...(totalTokens === undefined ? {} : { totalTokens }),
    };
    const promptMs = finiteNonnegative(event.timings?.prompt_ms);
    const predictedMs = finiteNonnegative(event.timings?.predicted_ms);
    this.timings = {
      ...this.timings,
      ...(promptMs === undefined ? {} : { promptMs }),
      ...(predictedMs === undefined ? {} : { predictedMs }),
    };
  }

  observeUpstreamStatus(status: number): void {
    if (!this.finished) this.upstreamStatus = status;
  }

  serverTiming(): string | undefined {
    if (this.input.streaming) return undefined;
    const metrics = [
      ...(this.input.queueWaitMs === undefined
        ? []
        : [serverTimingMetric("queue", this.input.queueWaitMs)]),
      ...(this.input.structuredOutput
        ? [
            serverTimingMetric(
              "prepare",
              this.input.structuredOutput.preparationDurationMs,
            ),
          ]
        : []),
      ...(this.timings.promptMs === undefined
        ? []
        : [serverTimingMetric("prompt", this.timings.promptMs)]),
      ...(this.timings.predictedMs === undefined
        ? []
        : [serverTimingMetric("generation", this.timings.predictedMs)]),
    ];
    return metrics.length ? metrics.join(", ") : undefined;
  }

  observeStructuredOutputValidation(
    result: StructuredOutputValidationTelemetry,
  ): void {
    if (!this.input.structuredOutput || this.finished) return;
    this.structuredOutputValidation = result.outcome;
    const reasons = [...new Set(result.skipReasons)].sort();
    this.structuredOutputSkipReasons = reasons.length
      ? reasons.join(",")
      : undefined;
  }

  finish(terminal: InferenceTerminal): void {
    if (this.finished) return;
    this.finished = true;
    const totalMs = Math.max(0, performance.now() - this.input.startedAt);
    const attributes: Record<string, string | number | boolean> = {
      model_id: this.input.metadata.modelId,
      ...(this.input.metadata.catalog
        ? {
            artifact_revision: this.input.metadata.catalog.artifactRevision,
            quantization: this.input.metadata.catalog.quantization,
          }
        : {}),
      runtime_name: this.input.metadata.runtimeName,
      admission_active: this.input.metadata.admission.active,
      admission_slots: this.input.metadata.admission.slots,
      admission_waiting: this.input.metadata.admission.waiting,
      outcome: terminal.outcome,
      http_status: terminal.httpStatus,
      ...(this.upstreamStatus === undefined
        ? {}
        : { upstream_status: this.upstreamStatus }),
      total_duration_ms: Number(totalMs.toFixed(2)),
      ...(this.input.queueWaitMs === undefined
        ? {}
        : { queue_wait_ms: Number(this.input.queueWaitMs.toFixed(2)) }),
    };
    this.input.span.setAttribute(
      "localbase.inference.total.duration_ms",
      totalMs,
    );
    this.input.span.setAttribute(
      "localbase.inference.outcome",
      terminal.outcome,
    );
    this.input.span.setAttribute(
      "http.response.status_code",
      terminal.httpStatus,
    );
    if (this.upstreamStatus !== undefined) {
      this.input.span.setAttribute(
        "localbase.inference.upstream.status_code",
        this.upstreamStatus,
      );
    }
    if (terminal.source) {
      attributes.terminal_source = terminal.source;
      this.input.span.setAttribute(
        "localbase.inference.terminal.source",
        terminal.source,
      );
    }
    if (this.input.structuredOutput && this.structuredOutputValidation) {
      attributes.json_schema_requested = true;
      attributes.json_schema_native_mode = "requested";
      attributes.json_schema_preparation_ms = Number(
        this.input.structuredOutput.preparationDurationMs.toFixed(2),
      );
      attributes.json_schema_validation = this.structuredOutputValidation;
      this.input.span.setAttribute(
        "localbase.inference.json_schema.validation",
        this.structuredOutputValidation,
      );
      if (this.structuredOutputSkipReasons) {
        attributes.json_schema_skip_reasons = this.structuredOutputSkipReasons;
        this.input.span.setAttribute(
          "localbase.inference.json_schema.skip_reasons",
          this.structuredOutputSkipReasons,
        );
      }
    }
    if (this.usage.promptTokens !== undefined) {
      attributes.prompt_tokens = this.usage.promptTokens;
      this.input.span.setAttribute(
        "gen_ai.usage.input_tokens",
        this.usage.promptTokens,
      );
    }
    if (this.usage.completionTokens !== undefined) {
      attributes.completion_tokens = this.usage.completionTokens;
      this.input.span.setAttribute(
        "gen_ai.usage.output_tokens",
        this.usage.completionTokens,
      );
    }
    if (this.usage.totalTokens !== undefined) {
      attributes.total_tokens = this.usage.totalTokens;
      this.input.span.setAttribute(
        "localbase.inference.usage.total_tokens",
        this.usage.totalTokens,
      );
    }
    if (this.timings.promptMs !== undefined) {
      attributes.prompt_duration_ms = this.timings.promptMs;
      this.input.span.setAttribute(
        "localbase.inference.backend.prompt.duration_ms",
        this.timings.promptMs,
      );
    }
    if (this.timings.predictedMs !== undefined) {
      attributes.predicted_duration_ms = this.timings.predictedMs;
      this.input.span.setAttribute(
        "localbase.inference.backend.predicted.duration_ms",
        this.timings.predictedMs,
      );
    }
    if (this.finishReasons.size) {
      const finishReasons = [...this.finishReasons].sort().join(",");
      attributes.finish_reasons = finishReasons;
      this.input.span.setAttribute(
        "localbase.inference.finish_reasons",
        finishReasons,
      );
    }
    if (terminal.outcome !== "completed")
      this.input.span.setStatus({ code: SpanStatusCode.ERROR });
    this.input.logger.event({
      severity: terminal.outcome === "completed" ? "info" : "warn",
      eventName: "inference.completed",
      category: "runtime",
      component: "inference",
      runtime: this.input.metadata.modality,
      message: "Inference response settled.",
      requestId: this.input.requestId,
      attributes,
    });
    this.input.span.end();
  }
}
