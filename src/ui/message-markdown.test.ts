import { expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MessageMarkdown, statusFlash } from "./message-markdown";

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
