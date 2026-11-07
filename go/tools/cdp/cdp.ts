// The DevTools client the browser checks under tools/ share: chat-runs-check, phone-check,
// axe-check, mic-check, ui-shots and e2e. It holds what each of them used to carry its own copy
// of: the connection to Chrome, commands and their answers, events, an optional time limit per
// command, and what happens when the browser goes away. Opening has a time limit of its own, so
// a browser that never answers ends the run with status 2 rather than hanging it. Each check
// keeps its own words for a failure, through the options.
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
  // How long opening may take, in milliseconds: asking for a tab, then the connection itself.
  // 30 s by default. Chrome answers in milliseconds once it is up, and the scripts that start it
  // wait for it to answer first.
  openTimeoutMs?: number;
  // What happens when opening fails or runs out of time. By default the run stops with status 2
  // and says what failed. With rejectOpenError, the promise rejects with that message instead,
  // for a check that ends the run its own way.
  onOpenFailure?: (message: string) => void;
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
const OPEN_MS = 30_000;

// openFailed ends an open that failed: by default it stops the run with status 2 and says why,
// since nothing in a check can go on without its browser. It throws whenever it returns.
function openFailed(options: Options, message: string): never {
  if (!options.rejectOpenError) {
    if (options.onOpenFailure) options.onOpenFailure(message);
    else { console.error(message); process.exit(2); }
  }
  throw new Error(message);
}

// connect opens a DevTools WebSocket: a page's, or the browser's own for a check that opens its
// own targets and talks to them through sessions.
export async function connect(url: string, options: Options = {}): Promise<Client> {
  const ms = options.openTimeoutMs ?? OPEN_MS;
  const ws = new WebSocket(url);
  try {
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`it didn't open in ${ms / 1000} s`)), ms);
      ws.addEventListener("open", () => { clearTimeout(timer); resolve(); }, { once: true });
      ws.addEventListener("error", () => { clearTimeout(timer); reject(new Error("the connection failed")); }, { once: true });
    });
  } catch (e) {
    ws.close();
    openFailed(options, `could not connect to Chrome's DevTools at ${url}: ${(e as Error).message}`);
  }
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
  const ms = options.openTimeoutMs ?? OPEN_MS;
  let target: any;
  try {
    const answer = await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT", signal: AbortSignal.timeout(ms) });
    target = await answer.json();
  } catch (e) {
    const why = (e as Error).name === "TimeoutError" ? `no answer in ${ms / 1000} s` : (e as Error).message;
    openFailed(options, `could not open a tab through Chrome's DevTools at ${cdpBase}: ${why}`);
  }
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
