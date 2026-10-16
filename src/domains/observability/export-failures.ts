export type OtelDiagnosticSeverity = "info" | "warn" | "error";

export type OtelDiagnostic = (
  severity: OtelDiagnosticSeverity,
  message: string,
  attributes?: Record<string, unknown>,
) => void;

/** Upper bound on how often one failing channel logs while an outage persists. */
export const OTEL_FAILURE_SUMMARY_INTERVAL_MS = 10 * 60_000;

export type ExportFailureReport = {
  signal: string;
  severity: "warn" | "error";
  message: string;
  summaryMessage?: string;
  detail: string;
  items?: number;
  attributes?: Record<string, unknown>;
};

type Outage = {
  signal: string;
  summaryMessage: string;
  since: number;
  lastFailureAt: number;
  lastReportAt: number;
  detail: string;
  latestDetail: string;
  failures: number;
  suppressedFailures: number;
  suppressedItems: number;
};

/**
 * Collapses repeated failures into one event per outage. Each channel logs its
 * first failure with full detail, counts repeats, and emits at most one
 * summary per interval. An outage ends when its channel recovers or stays
 * quiet for a full interval. Summaries are emitted on the next failure or at
 * shutdown, so no timer keeps the process alive and no counted failure goes
 * unreported.
 */
export class ExportFailureReporter {
  private readonly outages = new Map<string, Outage>();

  constructor(
    private readonly diagnostic: OtelDiagnostic,
    private readonly now: () => number = Date.now,
    private readonly summaryIntervalMs = OTEL_FAILURE_SUMMARY_INTERVAL_MS,
  ) {}

  failing(channel: string): boolean {
    return this.outages.has(channel);
  }

  failure(channel: string, report: ExportFailureReport): void {
    const now = this.now();
    let outage = this.outages.get(channel);
    if (outage && now - outage.lastFailureAt >= this.summaryIntervalMs) {
      if (outage.suppressedFailures > 0) {
        this.summarize(outage, outage.lastFailureAt);
      }
      this.outages.delete(channel);
      outage = undefined;
    }
    if (!outage) {
      this.outages.set(channel, {
        signal: report.signal,
        summaryMessage: report.summaryMessage ?? "OTLP export still failing.",
        since: now,
        lastFailureAt: now,
        lastReportAt: now,
        detail: report.detail,
        latestDetail: report.detail,
        failures: 1,
        suppressedFailures: 0,
        suppressedItems: 0,
      });
      this.diagnostic(report.severity, report.message, {
        signal: report.signal,
        ...report.attributes,
        detail: report.detail,
      });
      return;
    }
    outage.lastFailureAt = now;
    outage.failures++;
    outage.suppressedFailures++;
    outage.suppressedItems += report.items ?? 0;
    outage.latestDetail = report.detail;
    if (now - outage.lastReportAt >= this.summaryIntervalMs) {
      this.summarize(outage, now);
    }
  }

  recovered(channel: string): void {
    const outage = this.outages.get(channel);
    if (!outage) return;
    this.outages.delete(channel);
    this.diagnostic(
      "info",
      "OTLP export recovered.",
      this.outageAttributes(outage, this.now()),
    );
  }

  /** Reports suppressed failures that no summary or recovery has covered yet. */
  flush(): void {
    const now = this.now();
    for (const outage of this.outages.values()) {
      if (outage.suppressedFailures > 0) this.summarize(outage, now);
    }
  }

  private summarize(outage: Outage, now: number): void {
    this.diagnostic(
      "warn",
      outage.summaryMessage,
      this.outageAttributes(outage, now),
    );
    outage.lastReportAt = now;
    outage.suppressedFailures = 0;
    outage.suppressedItems = 0;
  }

  private outageAttributes(
    outage: Outage,
    now: number,
  ): Record<string, unknown> {
    return {
      signal: outage.signal,
      failingForMs: now - outage.since,
      failures: outage.failures,
      suppressedFailures: outage.suppressedFailures,
      ...(outage.suppressedItems > 0
        ? { suppressedItems: outage.suppressedItems }
        : {}),
      detail: outage.detail,
      ...(outage.latestDetail !== outage.detail
        ? { latestDetail: outage.latestDetail }
        : {}),
    };
  }
}
