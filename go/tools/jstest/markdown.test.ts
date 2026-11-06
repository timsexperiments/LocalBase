// The Go UI's markdown subset against react-markdown with GFM, the parser
// the TS UI renders replies with, plus the safety rules.
import { describe, expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import Markdown from "react-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { gfm } from "micromark-extension-gfm";
import { parse } from "../../internal/web/static/js/markdown.js";

// react-markdown with GFM, as message-markdown.tsx configures it.
function remarkGfm(this: any) {
  const data = this.data();
  (data.micromarkExtensions ??= []).push(gfm());
  (data.fromMarkdownExtensions ??= []).push(gfmFromMarkdown());
}
const reference = (md: string) => renderToStaticMarkup(createElement(Markdown, { remarkPlugins: [remarkGfm] }, md));

const esc = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
function inlineHTML(list: any[]): string {
  return list.map((n) => {
    switch (n.t) {
      case "text": return esc(n.v);
      case "code": return `<code>${esc(n.v)}</code>`;
      case "strong": return `<strong>${inlineHTML(n.c)}</strong>`;
      case "em": return `<em>${inlineHTML(n.c)}</em>`;
      case "del": return `<del>${inlineHTML(n.c)}</del>`;
      case "br": return "<br/>";
      case "link": return `<a href="${esc(n.href)}">${inlineHTML(n.c)}</a>`;
    }
    return "";
  }).join("");
}
function html(list: any[], tight = false): string {
  return list.map((b) => {
    switch (b.t) {
      case "p": return tight ? inlineHTML(b.c) : `<p>${inlineHTML(b.c)}</p>`;
      case "h": return `<h${b.level}>${inlineHTML(b.c)}</h${b.level}>`;
      case "hr": return "<hr/>";
      case "code": return `<pre><code${b.lang ? ` class="language-${b.lang}"` : ""}>${esc(b.text)}${b.text ? "\n" : ""}</code></pre>`;
      case "quote": return `<blockquote>${html(b.c)}</blockquote>`;
      case "list": {
        const tag = b.ordered ? "ol" : "ul";
        const start = b.ordered && b.start !== 1 ? ` start="${b.start}"` : "";
        return `<${tag}${start}>${b.items.map((it: any[]) => `<li>${!b.loose && it[0]?.t === "p" ? inlineHTML(it[0].c) + (it.length > 1 ? "\n" : "") + html(it.slice(1)) : html(it)}</li>`).join("")}</${tag}>`;
      }
      case "table": {
        const a = (k: number) => (b.align[k] ? ` style="text-align:${b.align[k]}"` : "");
        return `<table><thead><tr>${b.head.map((h: any[], k: number) => `<th${a(k)}>${inlineHTML(h)}</th>`).join("")}</tr></thead>` +
          (b.rows.length ? `<tbody>${b.rows.map((r: any[][]) => `<tr>${r.map((c, k) => `<td${a(k)}>${inlineHTML(c)}</td>`).join("")}</tr>`).join("")}</tbody>` : "") + "</table>";
      }
    }
    return "";
  }).join("");
}
const norm = (s: string) => s.replace(/>\s+</g, "><").replace(/\n/g, "\\n").trim();

const fixtures = [
  "Hello **world** and *you* and `code` and ~~gone~~.",
  "# Title\n\nSome text.\n\n## Second\n\n### Third",
  "```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```",
  "```\nplain\n```",
  "- one\n- two\n- three",
  "1. first\n2. second\n3. third",
  "3. starts at three\n4. four",
  "- parent\n  - child\n  - child two\n- next",
  "> quoted\n> lines",
  "| a | b |\n|---|:-:|\n| 1 | 2 |\n| 3 | 4 |",
  "| left | right |\n|:--|--:|\n| x | y |",
  "A [link](https://example.com) and <https://example.org>.",
  "See https://example.com/path?q=1.",
  "Line one  \nLine two\\\nLine three",
  "---",
  "Escaped \\*not em\\* and \\`not code\\`.",
  "snake_case_name stays and __strong__ works",
  "Paragraph one\nstill one.\n\nParagraph two.",
  "Text then\n```js\ncode()\n```\nafter",
  "**bold *nested em* bold**",
  "1. item\n\n   continued paragraph\n2. next",
];

describe("matches react-markdown with GFM", () => {
  for (const md of fixtures)
    test(JSON.stringify(md).slice(0, 70), () => expect(norm(html(parse(md)))).toBe(norm(reference(md))));
});

describe("safety", () => {
  test("javascript: and data: links lose their link", () => {
    for (const md of ["[x](javascript:alert(1))", "[x](data:text/html,hi)", "[x](JAVASCRIPT:alert(1))", "[x](//evil.example)"])
      expect(JSON.stringify(parse(md))).not.toContain('"t":"link"');
  });
  test("raw HTML stays text", () => {
    const blocks = parse("<img src=x onerror=alert(1)> <script>alert(1)</script>");
    expect(blocks).toEqual([{ t: "p", c: [{ t: "text", v: "<img src=x onerror=alert(1)> <script>alert(1)</script>" }] }]);
  });
  test("an unterminated fence (a streaming reply) is still a code block", () => {
    expect(parse("Intro\n\n```py\nprint(1)")).toEqual([
      { t: "p", c: [{ t: "text", v: "Intro" }] },
      { t: "code", lang: "py", text: "print(1)" },
    ]);
  });
  test("long input parses quickly", () => {
    const md = ("*a " + "b ".repeat(2000) + "\n").repeat(50) + "_".repeat(5000);
    const start = performance.now();
    parse(md);
    expect(performance.now() - start).toBeLessThan(500);
  });
});
