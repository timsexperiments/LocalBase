// The corpus of markdown.test.ts, shared by the test (the Go side) and the fixture generator
// (react-markdown with GFM, the frozen TS side). See frozen.ts.

export const corpus = [
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

// norm makes two renderings of the same markup compare equal: whitespace between tags, and newlines.
export const norm = (s: string) => s.replace(/>\s+</g, "><").replace(/\n/g, "\\n").trim();
