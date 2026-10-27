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
