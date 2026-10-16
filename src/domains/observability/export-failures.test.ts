import { describe, expect, test } from "bun:test";
import { ExportResultCode, type ExportResult } from "@opentelemetry/core";
import {
  ExportFailureReporter,
  OTEL_FAILURE_SUMMARY_INTERVAL_MS,
  type OtelDiagnosticSeverity,
} from "./export-failures";
import { exportFailureEchoSignal, MonitoredExporter } from "./otel";

type Recorded = {
  severity: OtelDiagnosticSeverity;
  message: string;
  attributes?: Record<string, unknown>;
};

function harness() {
  let clock = 1_000_000;
  const events: Recorded[] = [];
  const reporter = new ExportFailureReporter(
    (severity, message, attributes) =>
      events.push({ severity, message, attributes }),
    () => clock,
  );
  return {
    events,
    reporter,
    advance(milliseconds: number) {
      clock += milliseconds;
    },
  };
}

function logsFailure(detail = "collector unreachable (ConnectionRefused)") {
  return {
    signal: "logs",
    severity: "warn" as const,
    message: "OTLP export failed.",
    detail,
    items: 2,
    attributes: { batchSize: 2 },
  };
}

describe("export failure reporter", () => {
  test("logs the first failure with detail and suppresses repeats", () => {
    const { events, reporter, advance } = harness();
    reporter.failure("logs", logsFailure());
    for (let index = 0; index < 50; index++) {
      advance(1_000);
      reporter.failure("logs", logsFailure());
    }
    expect(events).toEqual([
      {
        severity: "warn",
        message: "OTLP export failed.",
        attributes: {
          signal: "logs",
          batchSize: 2,
          detail: "collector unreachable (ConnectionRefused)",
        },
      },
    ]);
    expect(reporter.failing("logs")).toBe(true);
  });

  test("summarizes suppressed failures at most once per interval", () => {
    const { events, reporter, advance } = harness();
    reporter.failure("logs", logsFailure());
    // One failure per second for 25 minutes.
    for (let second = 1; second <= 25 * 60; second++) {
      advance(1_000);
      reporter.failure(
        "logs",
        logsFailure(
          second > 700 ? "collector responded with HTTP 503" : undefined,
        ),
      );
    }
    expect(events).toHaveLength(3);
    expect(events[1]).toEqual({
      severity: "warn",
      message: "OTLP export still failing.",
      attributes: {
        signal: "logs",
        failingForMs: OTEL_FAILURE_SUMMARY_INTERVAL_MS,
        failures: 601,
        suppressedFailures: 600,
        suppressedItems: 1_200,
        detail: "collector unreachable (ConnectionRefused)",
      },
    });
    expect(events[2]?.attributes).toEqual({
      signal: "logs",
      failingForMs: 2 * OTEL_FAILURE_SUMMARY_INTERVAL_MS,
      failures: 1_201,
      suppressedFailures: 600,
      suppressedItems: 1_200,
      detail: "collector unreachable (ConnectionRefused)",
      latestDetail: "collector responded with HTTP 503",
    });
  });

  test("logs recovery once at info with the uncovered count", () => {
    const { events, reporter, advance } = harness();
    reporter.recovered("logs");
    expect(events).toEqual([]);
    reporter.failure("logs", logsFailure());
    advance(30_000);
    reporter.failure("logs", logsFailure());
    advance(30_000);
    reporter.recovered("logs");
    reporter.recovered("logs");
    expect(events).toHaveLength(2);
    expect(events[1]).toEqual({
      severity: "info",
      message: "OTLP export recovered.",
      attributes: {
        signal: "logs",
        failingForMs: 60_000,
        failures: 2,
        suppressedFailures: 1,
        suppressedItems: 2,
        detail: "collector unreachable (ConnectionRefused)",
      },
    });
    expect(reporter.failing("logs")).toBe(false);

    // A later outage starts a fresh episode with its own first event.
    reporter.failure("logs", logsFailure("collector responded with HTTP 500"));
    expect(events).toHaveLength(3);
    expect(events[2]?.attributes?.detail).toBe(
      "collector responded with HTTP 500",
    );
  });

  test("tracks channels independently", () => {
    const { events, reporter, advance } = harness();
    reporter.failure("logs", logsFailure());
    reporter.failure("traces", { ...logsFailure(), signal: "traces" });
    advance(1_000);
    reporter.recovered("traces");
    reporter.failure("logs", logsFailure());
    expect(
      events.map((event) => [event.message, event.attributes?.signal]),
    ).toEqual([
      ["OTLP export failed.", "logs"],
      ["OTLP export failed.", "traces"],
      ["OTLP export recovered.", "traces"],
    ]);
    expect(reporter.failing("logs")).toBe(true);
  });

  test("ends an outage after a quiet interval and reports its remainder", () => {
    const { events, reporter, advance } = harness();
    const sdk = {
      signal: "sdk",
      severity: "error" as const,
      message: "OpenTelemetry SDK diagnostic.",
      summaryMessage: "OpenTelemetry SDK diagnostic repeated.",
      detail: "first",
    };
    reporter.failure("sdk:error", sdk);
    advance(60_000);
    reporter.failure("sdk:error", { ...sdk, detail: "second" });
    advance(OTEL_FAILURE_SUMMARY_INTERVAL_MS);
    reporter.failure("sdk:error", { ...sdk, detail: "third" });
    expect(events).toEqual([
      {
        severity: "error",
        message: "OpenTelemetry SDK diagnostic.",
        attributes: { signal: "sdk", detail: "first" },
      },
      {
        severity: "warn",
        message: "OpenTelemetry SDK diagnostic repeated.",
        attributes: {
          signal: "sdk",
          failingForMs: 60_000,
          failures: 2,
          suppressedFailures: 1,
          detail: "first",
          latestDetail: "second",
        },
      },
      {
        severity: "error",
        message: "OpenTelemetry SDK diagnostic.",
        attributes: { signal: "sdk", detail: "third" },
      },
    ]);
  });

  test("flush reports only failures no event has covered", () => {
    const { events, reporter, advance } = harness();
    reporter.failure("logs", logsFailure());
    reporter.failure("traces", { ...logsFailure(), signal: "traces" });
    advance(5_000);
    reporter.failure("logs", logsFailure());
    reporter.flush();
    reporter.flush();
    expect(events).toHaveLength(3);
    expect(events[2]).toMatchObject({
      severity: "warn",
      message: "OTLP export still failing.",
      attributes: { signal: "logs", suppressedFailures: 1, failures: 2 },
    });
  });
});

describe("monitored exporter", () => {
  function exporter(results: ExportResult[]) {
    let clock = 0;
    const events: Recorded[] = [];
    const reporter = new ExportFailureReporter(
      (severity, message, attributes) =>
        events.push({ severity, message, attributes }),
      () => clock,
    );
    const forwarded: ExportResult[] = [];
    const monitored = new MonitoredExporter<number>(
      {
        export(_items, callback) {
          const result = results.shift();
          if (!result) throw new Error("synchronous rejection");
          callback(result);
        },
        async shutdown() {},
      },
      "traces",
      reporter,
    );
    return {
      events,
      forwarded,
      run(count: number) {
        for (let index = 0; index < count; index++) {
          clock += 1_000;
          monitored.export([1, 2, 3], (result) => forwarded.push(result));
        }
      },
    };
  }

  test("reports one outage, tags forwarded errors, and logs recovery", () => {
    const failed = {
      code: ExportResultCode.FAILED,
      error: new Error("collector unreachable (ConnectionRefused)"),
    };
    const { events, forwarded, run } = exporter([
      failed,
      failed,
      failed,
      { code: ExportResultCode.SUCCESS },
      { code: ExportResultCode.SUCCESS },
    ]);
    run(6);
    expect(events.map((event) => [event.severity, event.message])).toEqual([
      ["warn", "OTLP export failed."],
      ["info", "OTLP export recovered."],
      ["warn", "OTLP exporter rejected a batch."],
    ]);
    expect(events[0]?.attributes).toEqual({
      signal: "traces",
      batchSize: 3,
      detail: "collector unreachable (ConnectionRefused)",
    });
    expect(events[1]?.attributes).toMatchObject({
      failures: 3,
      suppressedFailures: 2,
      suppressedItems: 6,
    });
    expect(forwarded.map((result) => result.code)).toEqual([
      ExportResultCode.FAILED,
      ExportResultCode.FAILED,
      ExportResultCode.FAILED,
      ExportResultCode.SUCCESS,
      ExportResultCode.SUCCESS,
      ExportResultCode.FAILED,
    ]);
    const echo = JSON.stringify({ message: forwarded[0]?.error?.message });
    expect(exportFailureEchoSignal(echo)).toBe("traces");
    expect(
      exportFailureEchoSignal('{"message":"BatchSpanProcessor: Timeout"}'),
    ).toBeUndefined();
  });
});
