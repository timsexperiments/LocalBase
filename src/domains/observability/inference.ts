import { SpanStatusCode, type Span } from "@opentelemetry/api";
import { z } from "zod";
import type { ILogger } from "./logging";

type Usage = Readonly<{
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
}>;

type Timings = Readonly<{ promptMs: number; predictedMs: number }>;

export type InferenceOutcome = "completed" | "cancelled" | "error";

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
      .array(
        z
          .object({ finish_reason: z.string().nullable().optional() })
          .passthrough(),
      )
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

/** Records only validated, content-free backend completion metadata. */
export class InferenceTelemetry {
  private usage: Usage | undefined;
  private timings: Timings | undefined;
  private readonly finishReasons = new Set<string>();
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
      modelId: string;
      requestId: string;
      startedAt: number;
      queueWaitMs?: number;
      structuredOutput?: StructuredOutputTelemetry;
      logger: Pick<ILogger, "event">;
      span: Span;
    }>,
  ) {
    input.span.setAttribute("localbase.inference.model_id", input.modelId);
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

  observeValidatedChatEvent(value: unknown): void {
    const parsed = completionMetadataSchema.safeParse(value);
    if (!parsed.success) return;
    const event = parsed.data;
    for (const choice of event.choices ?? []) {
      if (typeof choice.finish_reason === "string")
        this.finishReasons.add(choice.finish_reason);
    }
    const promptTokens = finiteNonnegative(event.usage?.prompt_tokens);
    const completionTokens = finiteNonnegative(event.usage?.completion_tokens);
    const totalTokens = finiteNonnegative(event.usage?.total_tokens);
    if (
      promptTokens !== undefined &&
      completionTokens !== undefined &&
      totalTokens !== undefined
    ) {
      this.usage = { promptTokens, completionTokens, totalTokens };
    }
    const promptMs = finiteNonnegative(event.timings?.prompt_ms);
    const predictedMs = finiteNonnegative(event.timings?.predicted_ms);
    if (promptMs !== undefined && predictedMs !== undefined) {
      this.timings = { promptMs, predictedMs };
    }
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

  finish(outcome: InferenceOutcome): void {
    if (this.finished) return;
    this.finished = true;
    const totalMs = Math.max(0, performance.now() - this.input.startedAt);
    const attributes: Record<string, string | number | boolean> = {
      model_id: this.input.modelId,
      outcome,
      total_duration_ms: Number(totalMs.toFixed(2)),
      ...(this.input.queueWaitMs === undefined
        ? {}
        : { queue_wait_ms: Number(this.input.queueWaitMs.toFixed(2)) }),
    };
    this.input.span.setAttribute(
      "localbase.inference.total.duration_ms",
      totalMs,
    );
    this.input.span.setAttribute("localbase.inference.outcome", outcome);
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
    if (this.usage) {
      attributes.prompt_tokens = this.usage.promptTokens;
      attributes.completion_tokens = this.usage.completionTokens;
      attributes.total_tokens = this.usage.totalTokens;
      this.input.span.setAttribute(
        "gen_ai.usage.input_tokens",
        this.usage.promptTokens,
      );
      this.input.span.setAttribute(
        "gen_ai.usage.output_tokens",
        this.usage.completionTokens,
      );
    }
    if (this.timings) {
      attributes.prompt_duration_ms = this.timings.promptMs;
      attributes.predicted_duration_ms = this.timings.predictedMs;
      this.input.span.setAttribute(
        "localbase.inference.backend.prompt.duration_ms",
        this.timings.promptMs,
      );
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
    if (outcome !== "completed")
      this.input.span.setStatus({ code: SpanStatusCode.ERROR });
    this.input.logger.event({
      severity: outcome === "completed" ? "info" : "warn",
      eventName: "inference.completed",
      category: "runtime",
      component: "inference",
      runtime: "llm",
      message: "Inference response settled.",
      requestId: this.input.requestId,
      attributes,
    });
    this.input.span.end();
  }
}
