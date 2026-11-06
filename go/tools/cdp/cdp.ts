// The DevTools client the browser checks under tools/ share: chat-runs-check, phone-check,
// axe-check, mic-check, ui-shots and e2e. It holds what each of them used to carry its own copy
// of: the connection to Chrome, commands and their answers, events, an optional time limit per
// command, and what happens when the browser goes away. Each check keeps its own words for a
// failure, through the options, so what a check prints is what it printed before.
//
// Starting Chrome, and stopping it with everything it started, stays with the shell scripts
// (tools/browser-run.sh) and the suite's driver (tools/e2e/main.go).
//
//   import { evaluate, openPage } from "../cdp/cdp.ts";
//   const { send } = await openPage(cdpBase);
//   await send("Page.navigate", { url });
//   const title = await evaluate(send, "document.title");

export type Message = {
  id?: number;
  result?: any;
  error?: { code?: number; message: string; data?: string };
  method?: string;
  params?: any;
  sessionId?: string;
};

export type Send = (method: string, params?: any, sessionId?: string) => Promise<any>;

export type Options = {
  // What a failed command throws. By default "<method>: <Chrome's message>".
  errorText?: (method: string, message: string) => string;
  // How long a command waits for its answer, in milliseconds. Unset, it waits as long as it takes.
  timeoutMs?: number;
  // What a command that ran out of time throws. By default "<method>: no answer in <n> s".
  timeoutText?: (method: string, ms: number) => string;
  // What happens when the connection to the browser closes, once it has opened. By default the
  // run stops with status 2: with its browser gone, nothing here can finish.
  onClose?: () => void;
  // Whether a connection that fails while opening rejects. By default it waits for the open.
  rejectOpenError?: boolean;
};

export type Client = {
  ws: WebSocket;
  send: Send;
  // Every message that isn't the answer to a command: events, from the page or from a session.
  onEvent: (handler: (m: Message) => void) => void;
};

const stop = () => process.exit(2);
const chromeSaid = (method: string, message: string) => `${method}: ${message}`;
const noAnswer = (method: string, ms: number) => `${method}: no answer in ${ms / 1000} s`;

// connect opens a DevTools WebSocket: a page's, or the browser's own for a check that opens its
// own targets and talks to them through sessions.
export async function connect(url: string, options: Options = {}): Promise<Client> {
  const ws = new WebSocket(url);
  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    if (options.rejectOpenError) ws.addEventListener("error", reject, { once: true });
  });
  ws.addEventListener("close", options.onClose ?? stop);
  let id = 0;
  const waiting = new Map<number, (m: Message) => void>();
  const handlers: ((m: Message) => void)[] = [];
  ws.addEventListener("message", (e) => {
    const m: Message = JSON.parse(String(e.data));
    if (m.id && waiting.has(m.id)) {
      waiting.get(m.id)!(m);
      waiting.delete(m.id);
      return;
    }
    for (const handler of handlers) handler(m);
  });
  const errorText = options.errorText ?? chromeSaid;
  const timeoutText = options.timeoutText ?? noAnswer;
  const send: Send = (method, params = {}, sessionId) => new Promise<any>((resolve, reject) => {
    const n = ++id;
    const ms = options.timeoutMs;
    const timer = ms === undefined ? undefined : setTimeout(() => { waiting.delete(n); reject(new Error(timeoutText(method, ms))); }, ms);
    waiting.set(n, (m) => {
      if (timer !== undefined) clearTimeout(timer);
      if (m.error) reject(new Error(errorText(method, m.error.message)));
      else resolve(m.result);
    });
    ws.send(JSON.stringify({ id: n, method, params, ...(sessionId ? { sessionId } : {}) }));
  });
  return { ws, send, onEvent: (handler) => void handlers.push(handler) };
}

// openPage opens a new tab and connects to it, as every check but the suite does.
export async function openPage(cdpBase: string, options: Options = {}): Promise<Client> {
  const target = await (await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT" })).json();
  return connect(target.webSocketDebuggerUrl, options);
}

// What an exception in the page says: its description, else its text.
export const exceptionText = (details: any): string => details.exception?.description ?? details.text;

// evaluate runs an expression in the page, waits for a promise it returns, and gives back its
// value. An exception in the page throws, in the words describe gives it.
export async function evaluate(send: Send, expression: string, describe: (details: any) => string = exceptionText, sessionId?: string) {
  const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }, sessionId);
  if (r.exceptionDetails) throw new Error(describe(r.exceptionDetails));
  return r.result.value;
}
