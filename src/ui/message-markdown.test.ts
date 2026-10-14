import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MessageMarkdown } from "./message-markdown";

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
