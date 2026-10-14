import { isValidElement, memo, useRef, useState, type ReactNode } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { codeLanguage } from "./client";

export function Copy({
  text,
  label = "Copy",
}: {
  text: string | (() => string);
  label?: string;
}) {
  const [status, setStatus] = useState(label);
  return (
    <button
      type="button"
      className="copy"
      onClick={() => {
        if (!navigator.clipboard) {
          setStatus("Copy unavailable");
          return;
        }
        void navigator.clipboard
          .writeText(typeof text === "function" ? text() : text)
          .then(
            () => setStatus("Copied"),
            () => setStatus("Copy unavailable"),
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
const remarkPlugins = [remarkGfm];
/**
 * Message Markdown (GitHub-flavored: tables, strikethrough, task lists,
 * autolinks), memoized so a streaming reply re-renders only itself rather
 * than re-parsing every earlier (possibly very long) message per delta.
 */
export const MessageMarkdown = memo(function MessageMarkdown({
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
