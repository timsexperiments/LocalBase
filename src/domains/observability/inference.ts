import { SpanStatusCode, type Span } from "@opentelemetry/api";
import type { ILogger } from "./logging";

type Usage = Readonly<{
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
}>;

type Timings = Readonly<{ promptMs: number; predictedMs: number }>;

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

  constructor(
    private readonly input: Readonly<{
      modelId: string;
      requestId: string;
      startedAt: number;
      logger: Pick<ILogger, "event">;
      span?: Span;
    }>,
  ) {
    input.span?.setAttribute("localbase.inference.model_id", input.modelId);
    input.span?.setAttribute("localbase.request_id", input.requestId);
  }

  observeValidatedChatEvent(value: unknown): void {
    if (!value || typeof value !== "object" || "error" in value) return;
    const event = value as {
      choices?: Array<{ finish_reason?: string | null }>;
      usage?: {
        prompt_tokens?: unknown;
        completion_tokens?: unknown;
        total_tokens?: unknown;
      } | null;
      timings?: { prompt_ms?: unknown; predicted_ms?: unknown };
    };
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

  finish(outcome: "completed" | "cancelled" | "error"): void {
    const totalMs = Math.max(0, performance.now() - this.input.startedAt);
    const attributes: Record<string, string | number | boolean> = {
      model_id: this.input.modelId,
      outcome,
      total_duration_ms: Number(totalMs.toFixed(2)),
    };
    this.input.span?.setAttribute(
      "localbase.inference.total.duration_ms",
      totalMs,
    );
    this.input.span?.setAttribute("localbase.inference.outcome", outcome);
    if (this.usage) {
      attributes.prompt_tokens = this.usage.promptTokens;
      attributes.completion_tokens = this.usage.completionTokens;
      attributes.total_tokens = this.usage.totalTokens;
      this.input.span?.setAttribute(
        "gen_ai.usage.input_tokens",
        this.usage.promptTokens,
      );
      this.input.span?.setAttribute(
        "gen_ai.usage.output_tokens",
        this.usage.completionTokens,
      );
    }
    if (this.timings) {
      attributes.prompt_duration_ms = this.timings.promptMs;
      attributes.predicted_duration_ms = this.timings.predictedMs;
      this.input.span?.setAttribute(
        "localbase.inference.backend.prompt.duration_ms",
        this.timings.promptMs,
      );
      this.input.span?.setAttribute(
        "localbase.inference.backend.predicted.duration_ms",
        this.timings.predictedMs,
      );
    }
    if (this.finishReasons.size) {
      const finishReasons = [...this.finishReasons].sort().join(",");
      attributes.finish_reasons = finishReasons;
      this.input.span?.setAttribute(
        "localbase.inference.finish_reasons",
        finishReasons,
      );
    }
    if (outcome !== "completed")
      this.input.span?.setStatus({ code: SpanStatusCode.ERROR });
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
    this.input.span?.end();
  }
}
