// Golden log-event acceptance and diagnostics redaction from the TypeScript
// code.
//
//   bun run go/tools/golden/logs.ts > go/internal/obs/testdata/events.json
//
// Each line: whether the reader keeps it (logEventSchema), and the event a
// diagnostics bundle holds (redactLogEventForDiagnostics).
import { logEventSchema, redactLogEventForDiagnostics } from "../../../src/domains/observability/logging";

const base = {
  schemaVersion: 2, id: "0b7c4a62-0f3b-4f6f-9d0e-6a8a1d2c3e4f", timestamp: "2026-10-20T10:00:00.000Z", severity: "info",
  eventName: "gateway.request", category: "http", component: "gateway", runtime: "gateway", message: "Request served.",
};
const variants: Record<string, unknown>[] = [
  {},
  { message: "Authorization: Bearer abcdefghijklmnop token=hf_abcdefghijklmnop" },
  { message: 'payload {"prompt": "hello"}' },
  { message: "see https://user:pass@example.com/x?api_key=secret&ok=1" },
  { message: "cookie: session=abc; other=1" },
  { message: "x".repeat(2048) },
  { message: "x".repeat(2049) },
  { message: "" },
  { message: "\u001b[31mred\u001b[0m text" },
  { eventName: "token.refresh-abcdefghijklmnopqrstuvwxyz" },
  { eventName: "Bad.Name" },
  { component: "auth-token-abcdefghijklmnop" },
  { component: "Gateway" },
  { severity: "fatal" },
  { category: "other" },
  { runtime: "kokoro" },
  { id: "not-a-uuid" },
  { id: "00000000-0000-0000-0000-000000000000" },
  { timestamp: "2026-10-20 10:00:00" },
  { timestamp: "2026-10-20T10:00:00+02:00" },
  { timestamp: "2026-10-20T10:00Z" },
  { timestamp: "2026-13-20T10:00:00Z" },
  { requestId: "req-1:a.b_c" },
  { requestId: "bad id" },
  { trace: { traceId: "ab".repeat(16), spanId: "cd".repeat(8) } },
  { trace: { traceId: "0".repeat(32), spanId: "cd".repeat(8) } },
  { trace: { traceId: "AB".repeat(16), spanId: "cd".repeat(8) } },
  { http: { method: "POST", path: "/v1/chat/completions", status: 200, durationMs: 12.5 } },
  { http: { method: "POST", path: "/v1/other", status: 200, durationMs: 1 } },
  { http: { method: "POST", path: "/app/api/v1/models", status: 200, durationMs: 1 } },
  { http: { method: "TRACE", path: "/health", status: 200, durationMs: 1 } },
  { http: { method: "GET", path: "/health", status: 200.5, durationMs: 1 } },
  { http: { method: "GET", path: "unmatched-route", status: 404, durationMs: 3_600_001 } },
  { http: { method: "GET", path: "/health", status: 200, durationMs: 1, extra: 1 } },
  { error: { type: "Error", message: "spawn failed: password=hunter2" } },
  { error: { type: "Error", message: "failed", code: "E1" } },
  { error: { type: "", message: "failed" } },
  { attributes: { model_id: "qwen", api_key: "lb_abcdefghijklmnop", prompt_tokens: 12, completion_tokens: -1, ok: true,
    note: "Bearer abcdefghijkl", "a.b-c_d": 1.5, total_tokens: 3 } },
  { attributes: { Model: "x" } },
  { attributes: { model: { nested: true } } },
  { attributes: { model: "y".repeat(513) } },
  { attributes: Object.fromEntries(Array.from({ length: 33 }, (_, i) => [`k${i}`, i])) },
  { attributes: Object.fromEntries(Array.from({ length: 32 }, (_, i) => [`k${i}`, i])) },
  { attributes: { "2": "two", b: "b", "1": "one" } },
  { extra: true },
  { schemaVersion: 1 },
];
const lines = variants.map((v) => JSON.stringify({ ...base, ...v }));
lines.push("{", "[]", '"text"', JSON.stringify({ ...base, message: undefined }));
const cases = lines.map((line) => {
  let parsed;
  try {
    parsed = logEventSchema.safeParse(JSON.parse(line));
  } catch {
    return { line, accepted: false };
  }
  if (!parsed.success) return { line, accepted: false };
  return { line, accepted: true, diagnostics: JSON.stringify(redactLogEventForDiagnostics(parsed.data)) };
});
console.log(JSON.stringify(cases, null, 1));
