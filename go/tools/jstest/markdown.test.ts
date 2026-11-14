// The Go UI's markdown subset against react-markdown with GFM, the parser the TS UI rendered replies
// with, frozen as fixtures (parity/frozen.ts; testdata/markdown.json), plus the safety rules.
import { describe, expect, test } from "bun:test";
import { parse } from "../../internal/web/static/js/markdown.js";
import { corpus, norm } from "./parity/markdown.cases.ts";
import { fixture } from "./parity/frozen.ts";

// react-markdown's HTML for each of the corpus, through norm.
const reference = fixture("markdown").html as Record<string, string>;

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

describe("matches react-markdown with GFM", () => {
  for (const md of corpus)
    test(JSON.stringify(md).slice(0, 70), () => {
      expect(reference[md]).toBeString();
      expect(norm(html(parse(md)))).toBe(reference[md]);
    });
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
