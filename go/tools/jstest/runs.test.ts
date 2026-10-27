// The detached-runs client's event-stream parser.
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import { sseParser } from "../../internal/web/static/js/runs.js";

const stream = 'id: 1\nevent: status\ndata: {"status":"in_progress"}\n\n: ping\n\nid: 2\nevent: chunk\ndata: {"a":1}\n\nid: 3\nevent: end\ndata: {"status":"completed"}\n\n';

describe("sseParser", () => {
  test("whole stream: ids, events, data; comments skipped", () => {
    expect(sseParser()(stream)).toEqual([
      { id: "1", event: "status", data: '{"status":"in_progress"}' },
      { id: "2", event: "chunk", data: '{"a":1}' },
      { id: "3", event: "end", data: '{"status":"completed"}' },
    ]);
  });
  test("any chunking gives the same messages", () => {
    const whole = sseParser()(stream);
    for (let size = 1; size < 12; size++) {
      const parse = sseParser();
      const out = [];
      for (let i = 0; i < stream.length; i += size) out.push(...parse(stream.slice(i, i + size)));
      expect(out).toEqual(whole);
    }
  });
  test("CRLF line endings and multi-line data", () => {
    expect(sseParser()("data: a\r\ndata: b\r\n\r\n")).toEqual([{ id: "", event: "message", data: "a\nb" }]);
  });
  test("an incomplete message waits for its blank line", () => {
    const parse = sseParser();
    expect(parse("id: 4\ndata: x\n")).toEqual([]);
    expect(parse("\n")).toEqual([{ id: "4", event: "message", data: "x" }]);
  });
});

// runs.response() turns a run's events into a /v1 stream for streamText.
import { response, endMessage } from "../../internal/web/static/js/runs.js";
import { streamText } from "../../internal/web/static/js/chat-core.js";

const chunk = (text: string) => JSON.stringify({ choices: [{ index: 0, delta: { content: text } }] });
function fakeGateway(streams: string[][], run: any = null) {
  const calls: string[] = [];
  (globalThis as any).document = { hidden: false, addEventListener() {}, dispatchEvent() {} };
  (globalThis as any).localStorage = { m: new Map(), getItem(k: string) { return this.m.get(k) ?? null; }, setItem(k: string, v: string) { this.m.set(k, v); } };
  (globalThis as any).fetch = async (url: string, init: any = {}) => {
    const lastId = new Headers(init.headers).get("last-event-id") ?? "";
    calls.push(`${init.method ?? "GET"} ${url} ${lastId}`);
    if (url.endsWith("/events")) {
      const events = streams.shift() ?? [];
      return new Response(events.join(""), { headers: { "content-type": "text/event-stream" } });
    }
    return new Response(JSON.stringify(run ?? {}), { headers: { "content-type": "application/json" } });
  };
  return calls;
}
const ev = (id: number, name: string, data: string) => `id: ${id}\nevent: ${name}\ndata: ${data}\n\n`;

describe("runs.response", () => {
  test("chunks stream through and a completed run ends cleanly", async () => {
    fakeGateway([[ev(1, "status", '{"status":"in_progress"}'), ev(2, "chunk", chunk("Hello ")), ev(3, "chunk", chunk("there")), ev(4, "end", '{"status":"completed"}')]]);
    let text = "";
    let ended: any;
    await streamText(await response({ id: "run_1", after: "", onEnd: (r: any) => (ended = r) }), (t: string) => (text += t));
    expect(text).toBe("Hello there");
    expect(ended.status).toBe("completed");
  });
  test("a dropped stream resumes from the last event id", async () => {
    const calls = fakeGateway([[ev(1, "chunk", chunk("a"))], [ev(2, "chunk", chunk("b")), ev(3, "end", '{"status":"completed"}')]]);
    let text = "";
    await streamText(await response({ id: "run_2", after: "" }), (t: string) => (text += t));
    expect(text).toBe("ab");
    expect(calls.filter((c) => c.includes("/events"))).toEqual([
      "GET /app/api/_localbase/runs/run_2/events ",
      "GET /app/api/_localbase/runs/run_2/events 1",
    ]);
  });
  test("an interrupted run keeps its text and fails with the reason", async () => {
    fakeGateway([[ev(1, "chunk", chunk("partial")), ev(2, "end", '{"status":"interrupted","reason":"gateway_restart"}')]]);
    let text = "";
    let ended: any;
    const read = streamText(await response({ id: "run_3", after: "", onEnd: (r: any) => (ended = r) }), (t: string) => (text += t));
    await expect(read).rejects.toThrow("LocalBase restarted before the reply finished.");
    expect(text).toBe("partial");
    expect(ended.reason).toBe("gateway_restart");
  });
  test("an empty resume after a gateway restart ends with the run's state", async () => {
    fakeGateway([[ev(5, "chunk", chunk("x"))], []], { id: "run_4", status: "interrupted", reason: "gateway_restart", output: { content: "x" } });
    let ended: any;
    const read = streamText(await response({ id: "run_4", after: "", onEnd: (r: any) => (ended = r) }), () => {});
    await expect(read).rejects.toThrow();
    expect(ended).toEqual({ id: "run_4", status: "interrupted", reason: "gateway_restart" });
  });
  test("messages name no models and no numbers", () => {
    for (const r of [{ status: "cancelled" }, { reason: "memory_pressure" }, { reason: "max_duration" }, { reason: "output_limit" }, {}])
      expect(endMessage(r)).not.toMatch(/\d/);
  });
});

describe("snapshot after a gateway restart", () => {
  test("a resumed stream adds only what it had not delivered", async () => {
    fakeGateway([[ev(1, "chunk", chunk("Hel")), ev(2, "chunk", chunk("lo"))],
      [ev(3, "snapshot", JSON.stringify({ status: "interrupted", output: { content: "Hello world" } })), ev(4, "end", '{"status":"interrupted","reason":"gateway_restart"}')]]);
    let text = "";
    await expect(streamText(await response({ id: "run_5", after: "" }), (t: string) => (text += t))).rejects.toThrow();
    expect(text).toBe("Hello world");
  });
  test("a stream ahead of the checkpoint adds nothing", async () => {
    fakeGateway([[ev(1, "chunk", chunk("Hello world, more"))],
      [ev(3, "snapshot", JSON.stringify({ status: "interrupted", output: { content: "Hello world" } })), ev(4, "end", '{"status":"interrupted","reason":"gateway_restart"}')]]);
    let text = "";
    await expect(streamText(await response({ id: "run_6", after: "" }), (t: string) => (text += t))).rejects.toThrow();
    expect(text).toBe("Hello world, more");
  });
  test("re-attaching from the start gets the whole snapshot", async () => {
    fakeGateway([[ev(1, "snapshot", JSON.stringify({ status: "interrupted", output: { content: "All of it" } })), ev(2, "end", '{"status":"interrupted","reason":"gateway_restart"}')]]);
    let text = "";
    await expect(streamText(await response({ id: "run_7", after: "" }), (t: string) => (text += t))).rejects.toThrow();
    expect(text).toBe("All of it");
  });
});
