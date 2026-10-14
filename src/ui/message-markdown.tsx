import {
  isValidElement,
  memo,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
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
