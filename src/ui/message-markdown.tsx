import {
  Fragment,
  isValidElement,
  memo,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import Markdown from "react-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { gfm } from "micromark-extension-gfm";
import type { Processor } from "unified";
import { codeLanguage } from "./client";

export const COPY_STATUS_MS = 2000;

// Shows a status briefly, then restores the label. A newer status restarts
// the timer so a stale reset never cuts it short.
export function statusFlash(
  set: (status: string) => void,
  label: string,
  ms = COPY_STATUS_MS,
) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  return {
    show(status: string) {
      set(status);
      clearTimeout(timer);
      timer = setTimeout(() => set(label), ms);
    },
    dispose: () => clearTimeout(timer),
  };
}

export function Copy({
  text,
  label = "Copy",
  className = "copy",
}: {
  text: string | (() => string);
  label?: string;
  className?: string;
}) {
  const [status, setStatus] = useState(label);
  const [flash] = useState(() => statusFlash(setStatus, label));
  useEffect(() => flash.dispose, [flash]);
  return (
    <button
      type="button"
      className={className || undefined}
      onClick={() => {
        if (!navigator.clipboard) {
          flash.show("Copy unavailable");
          return;
        }
        void navigator.clipboard
          .writeText(typeof text === "function" ? text() : text)
          .then(
            () => flash.show("Copied"),
            () => flash.show("Copy unavailable"),
          );
      }}
    >
      {status}
    </button>
  );
}
function CodeBlock({ children }: { children?: ReactNode }) {
  const ref = useRef<HTMLPreElement>(null);
  const language = codeLanguage(
    isValidElement<{ className?: string }>(children)
      ? children.props.className
      : undefined,
  );
  return (
    <div className="code-block">
      <div className="code-block-header">
        <span className="code-language">{language ?? "code"}</span>
        <Copy label="Copy code" text={() => ref.current?.textContent ?? ""} />
      </div>
      <pre ref={ref}>{children}</pre>
    </div>
  );
}
/**
 * GFM tables scroll inside their own container so a wide table never widens
 * the conversation column (or the page) on a narrow phone viewport.
 */
function MarkdownTable({ children }: { children?: ReactNode }) {
  return (
    <div className="table-scroll">
      <table>{children}</table>
    </div>
  );
}
const markdownComponents = {
  pre: CodeBlock,
  table: MarkdownTable,
  img: () => null,
  a: ({ children, href }: { children?: ReactNode; href?: string }) => (
    <a href={href} target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  ),
};
/**
 * GFM parsing only (tables, strikethrough, task lists, autolinks). Unlike
 * `remark-gfm` it skips the Markdown serializer extensions, which keeps
 * `mdast-util-to-markdown` out of the browser bundle.
 */
function remarkGfmParse(this: Processor) {
  const data = this.data() as {
    micromarkExtensions?: unknown[];
    fromMarkdownExtensions?: unknown[];
  };
  (data.micromarkExtensions ??= []).push(gfm());
  (data.fromMarkdownExtensions ??= []).push(gfmFromMarkdown());
}
const remarkPlugins = [remarkGfmParse];

const fenceOpen = /^( {0,3})(`{3,}|~{3,})(.*)$/;
const fenceClose = /^ {0,3}(`{3,}|~{3,})[ \t]*$/;
const listMarker = /^(?:[-+*]|\d{1,9}[.)])(?:[ \t]|$)|^\d{1,9}$/;
// HTML block types 1-5 (<script>, <pre>, comments, <?...?>, <!...>) may span
// blank lines, so splitting stops at the first line that could open one.
const spanningHtml = /<(?:script|pre|style|textarea)|<!|<\?/i;
const blank = (line: string) => /^[ \t]*\r?$/.test(line);
function indentOf(line: string) {
  let width = 0;
  for (const char of line) {
    if (char === " ") width++;
    else if (char === "\t") width += 4 - (width % 4);
    else break;
  }
  return width;
}

/**
 * Splits Markdown into top-level blocks whose concatenation is `text`: each
 * piece renders the same on its own as it does inside the whole document, so
 * a streaming reply re-parses only its last (still-growing) block per delta.
 *
 * Splits happen only at a blank line outside any code fence that is followed
 * by an unindented line that cannot continue a list. Anything that can reach
 * across blank lines (link or footnote definitions, multi-line HTML blocks,
 * fences whose extent depends on list nesting) falls back to fewer pieces —
 * never to a different render.
 */
export function splitMarkdownBlocks(text: string): string[] {
  const blocks: string[] = [];
  let start = 0;
  let position = 0;
  let content = false;
  let afterBlank = false;
  let fence: { char: string; length: number; indent: number } | undefined;
  let stop = false;
  for (const raw of text.split(/(?<=\n)/)) {
    const line = raw.endsWith("\n") ? raw.slice(0, -1) : raw;
    const at = position;
    position += raw.length;
    if (fence) {
      // A fence indented 1-3 spaces may sit inside a list item, where a less
      // indented line ends the item (and the fence) instead of continuing it.
      if (fence.indent > 0 && !blank(line) && indentOf(line) < fence.indent) {
        stop = true;
        break;
      }
      const close = fenceClose.exec(line);
      if (close?.[1]?.[0] === fence.char && close[1].length >= fence.length) {
        fence = undefined;
      }
      continue;
    }
    // Definitions apply to references anywhere in the document.
    if (line.includes("]:")) return [text];
    if (blank(line)) {
      afterBlank = content;
      continue;
    }
    if (afterBlank && indentOf(line) === 0 && !listMarker.test(line)) {
      blocks.push(text.slice(start, at));
      start = at;
    }
    afterBlank = false;
    content = true;
    if (spanningHtml.test(line)) {
      stop = true;
      break;
    }
    const open = fenceOpen.exec(line);
    if (open?.[2] && !(open[2][0] === "`" && open[3]?.includes("`"))) {
      fence = {
        char: open[2][0]!,
        length: open[2].length,
        indent: open[1]!.length,
      };
    }
  }
  if (stop && text.includes("]:", start)) return [text];
  blocks.push(text.slice(start));
  return blocks;
}

export const MarkdownBlock = memo(function MarkdownBlock({
  text,
}: {
  text: string;
}) {
  return (
    <Markdown
      skipHtml
      remarkPlugins={remarkPlugins}
      components={markdownComponents}
    >
      {text}
    </Markdown>
  );
});

/**
 * Message Markdown (GitHub-flavored), memoized so a streaming reply
 * re-renders only itself, and rendered block by block so each delta re-parses
 * only the reply's last block instead of the whole (possibly very long) text.
 * Top-level blocks are separated by a newline, as in a single render.
 */
export const MessageMarkdown = memo(function MessageMarkdown({
  text,
}: {
  text: string;
}) {
  return splitMarkdownBlocks(text).map((block, index) => (
    <Fragment key={index}>
      {index > 0 && "\n"}
      <MarkdownBlock text={block} />
    </Fragment>
  ));
});
