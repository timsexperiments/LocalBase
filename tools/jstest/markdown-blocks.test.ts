// Chat replies with real newlines parse into blocks (no React needed; the
// reference comparison is markdown.test.ts).
//
//   bun test go/tools/jstest/markdown-blocks.test.ts
import { describe, expect, test } from "bun:test";
import { parse } from "../../internal/web/static/js/markdown.js";

const reply = "Intro:\n\n- one\n- two\n\n```js\nx()\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nSee `code` and [link](https://example.com).";

describe("reply blocks", () => {
  const blocks = parse(reply);
  test("paragraph, list, code fence, table, paragraph", () => {
    expect(blocks.map((b: any) => b.t)).toEqual(["p", "list", "code", "table", "p"]);
  });
  test("the list has its two items", () => {
    expect(blocks[1].items.length).toBe(2);
  });
  test("inline code and links stay inline", () => {
    expect(blocks[4].c.map((n: any) => n.t)).toEqual(["text", "code", "text", "link", "text"]);
  });
  test("the same text on one line (newlines lost) is one paragraph", () => {
    expect(parse(reply.replace(/\s+/g, " ")).length).toBe(1);
  });
});
