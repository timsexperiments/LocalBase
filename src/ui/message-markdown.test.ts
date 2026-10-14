import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  MarkdownBlock,
  MessageMarkdown,
  splitMarkdownBlocks,
  statusFlash,
} from "./message-markdown";

const render = (text: string) =>
  renderToStaticMarkup(createElement(MessageMarkdown, { text }));

test("renders GFM tables inside a horizontal scroll container", () => {
  const markup = render(
    [
      "| Model | Params | Context | Notes |",
      "| --- | ---: | :---: | --- |",
      "| Qwen3.5 | 9B | 128k | **fast** |",
      "| Other | 27B | 32k | `slow` |",
    ].join("\n"),
  );
  expect(markup).toContain('<div class="table-scroll"><table>');
  expect(markup).toContain("<thead>");
  expect(markup).toContain("<th>Model</th>");
  expect(markup).toContain("<td>Qwen3.5</td>");
  expect(markup).toContain("<strong>fast</strong>");
  expect(markup.match(/<tr>/g)).toHaveLength(3);
  expect(markup).not.toContain("| Model |");
});

test("renders a streaming partial table without breaking", () => {
  expect(render("| Model | Params |\n| --- | --- |\n| Qwen")).toContain(
    '<div class="table-scroll"><table>',
  );
  // A header without its delimiter row yet is still a plain paragraph.
  expect(render("| Model | Params |")).toBe("<p>| Model | Params |</p>");
});

test("renders other GFM syntax and keeps raw HTML and images suppressed", () => {
  const markup = render(
    "~~old~~ https://example.com\n\n- [x] done\n- [ ] todo\n\n<b>raw</b> ![x](https://example.com/x.png)",
  );
  expect(markup).toContain("<del>old</del>");
  expect(markup).toContain(
    '<a href="https://example.com" target="_blank" rel="noreferrer noopener">https://example.com</a>',
  );
  expect(markup).toContain('type="checkbox"');
  expect(markup).not.toContain("<b>");
  expect(markup).not.toContain("<img");
});

test("copy status returns to the label after a short delay", async () => {
  const seen: string[] = [];
  const flash = statusFlash((status) => seen.push(status), "Copy", 20);
  flash.show("Copied");
  await Bun.sleep(10);
  // A second copy restarts the timer instead of being cut short.
  flash.show("Copied");
  await Bun.sleep(15);
  expect(seen).toEqual(["Copied", "Copied"]);
  await Bun.sleep(15);
  expect(seen).toEqual(["Copied", "Copied", "Copy"]);

  flash.show("Copy unavailable");
  flash.dispose();
  await Bun.sleep(30);
  expect(seen.at(-1)).toBe("Copy unavailable");
});

const whole = (text: string) =>
  renderToStaticMarkup(createElement(MarkdownBlock, { text }));

// Shapes that interact across blank lines: fences (also inside lists and
// with blank lines inside), tables, loose lists, quotes, HTML, definitions.
const fixtures = [
  "# Plan\n\nIntro with **bold** and `code`.\n\n## Steps\n\n1. First step\n2. Second step\n\n   Continued under it.\n\n3. Third\n\nAfter the list.\n",
  "Code:\n\n```ts\nconst a = 1;\n\nconst b = 2;\n```\n\nText after.\n\n~~~\nx\n\n```\nstill inside\n~~~\n\nDone.",
  "1. Install:\n\n   ```bash\n   bun install\n\n   bun run build\n   ```\n\n2. Run it\n\nOutro.",
  "- a\n  ```\n\nfoo\n```\nbar\n\nbaz\n```\n\nend",
  "  ```\n  indented fence\n\nback at zero\n\n  ```\n\nafter",
  "| A | B |\n| --- | --- |\n| 1 | 2 |\n\n| C |\n| - |\n| 3 |\n\nText.",
  "> quote one\n>\n> more\n\n> quote two\n\nplain\n\n    indented code\n\n    more code\n\nend",
  "See [the docs][d] and [^1].\n\nMore text.\n\n[d]: https://example.com\n\n[^1]: A note.\n",
  "Before.\n\n<!-- a comment\n\nstill comment -->\n\nAfter.\n\n<pre>\n\nx\n</pre>\n\nend",
  "- one\n\n- two\n\n+ three\n\n10. ten\n\n7\n\nplain\n\n* star\n\n-\n\nnot a list",
  "Setext\n======\n\nPara\n---\n\n---\n\n***\n\n``` js `bad`\nnot a fence\n\nok\n\n````\n```\n\nstill code\n````\n\ndone",
  "Text\ttab\n\n\tTabbed code\n\nx\r\n\r\ny\r\n",
  "1. Step\n   ```bash\n   echo one\n\n  echo two\n   ```\n\nAfter.",
  "```\nunclosed\n\nfence to the end",
];

test("splitting into blocks renders exactly like one whole render at every prefix", () => {
  for (const fixture of fixtures) {
    for (let end = 0; end <= fixture.length; end++) {
      const text = fixture.slice(0, end);
      expect(splitMarkdownBlocks(text).join("")).toBe(text);
      expect(render(text)).toBe(whole(text));
    }
  }
});

test("random block mixes render exactly like one whole render", () => {
  const pieces = [
    "Para *one*.\n",
    "\n",
    "\n\n",
    "- item\n",
    "  nested text\n",
    "1. step\n",
    "   ```sh\n",
    "  ```\n",
    "```\n",
    "~~~\n",
    "| a | b |\n| - | - |\n| 1 | 2 |\n",
    "> quote\n",
    "    code\n",
    "# Head\n",
    "---\n",
    "<div>\n",
    "<!-- c\n",
    "-->\n",
    "[x]: /u\n",
    "[x] ref\n",
    "text\n",
  ];
  let seed = 7;
  const next = () => (seed = (seed * 1103515245 + 12345) % 2 ** 31) % 1000;
  for (let doc = 0; doc < 300; doc++) {
    let text = "";
    const count = 4 + (next() % 14);
    for (let index = 0; index < count; index++) {
      text += pieces[next() % pieces.length];
      expect(render(text)).toBe(whole(text));
    }
  }
});

test("splits plain top-level blocks and keeps fenced blank lines together", () => {
  expect(splitMarkdownBlocks("a\n\nb\n\n```\nx\n\ny\n```\n\nc")).toEqual([
    "a\n\n",
    "b\n\n",
    "```\nx\n\ny\n```\n\n",
    "c",
  ]);
  // A list continues across blank lines while its items do.
  expect(splitMarkdownBlocks("- a\n\n- b\n\n  more\n\nc")).toEqual([
    "- a\n\n- b\n\n  more\n\n",
    "c",
  ]);
  // Definitions can be referenced from any block, so nothing is split.
  expect(splitMarkdownBlocks("[a]\n\n[a]: /x")).toEqual(["[a]\n\n[a]: /x"]);
});
