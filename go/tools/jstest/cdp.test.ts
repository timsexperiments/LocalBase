// The DevTools client the browser checks share (tools/cdp/cdp.ts), against a local WebSocket
// server that plays Chrome's part.
//
//   bun test go/tools/jstest
import { afterEach, describe, expect, test } from "bun:test";
import { connect, evaluate, openPage, type Message } from "../cdp/cdp.ts";

type Fake = { url: string; base: string; got: any[]; reply: (m: any) => void; close: () => void; stop: () => void };

// fake answers each command with answer(command), or not at all when that returns undefined.
// The port is the system's choice, as httptest's are in the Go tests.
function fake(answer: (m: any) => any): Fake {
  const got: any[] = [];
  let peer: any;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(req, srv) {
      const u = new URL(req.url);
      if (u.pathname === "/json/new") {
        got.push({ http: req.method, path: u.pathname + u.search });
        return Response.json({ webSocketDebuggerUrl: `ws://127.0.0.1:${server.port}/devtools/page/1` });
      }
      if (srv.upgrade(req)) return;
      return new Response("no", { status: 404 });
    },
    websocket: {
      open(ws) { peer = ws; },
      message(ws, data) {
        const m = JSON.parse(String(data));
        got.push(m);
        const a = answer(m);
        if (a !== undefined) ws.send(JSON.stringify({ id: m.id, ...a }));
      },
    },
  });
  return {
    url: `ws://127.0.0.1:${server.port}/devtools/browser/1`,
    base: `http://127.0.0.1:${server.port}`,
    got,
    reply: (m) => peer.send(JSON.stringify(m)),
    close: () => peer.close(),
    stop: () => server.stop(true),
  };
}

const servers: Fake[] = [];
const serve = (answer: (m: any) => any) => { const f = fake(answer); servers.push(f); return f; };
const keep = () => {}; // onClose for the tests: the default would end the test run
afterEach(() => { for (const s of servers.splice(0)) s.stop(); });

describe("send", () => {
  test("each command gets its own answer, whatever order they come in", async () => {
    const held: any[] = [];
    const f = serve((m) => { held.push(m); return undefined; });
    const { send } = await connect(f.url, { onClose: keep });
    const first = send("Page.enable");
    const second = send("Runtime.evaluate", { expression: "1" });
    while (held.length < 2) await Bun.sleep(5);
    f.reply({ id: held[1].id, result: { second: true } });
    f.reply({ id: held[0].id, result: { first: true } });
    expect(await first).toEqual({ first: true });
    expect(await second).toEqual({ second: true });
  });

  test("a command is sent as before: an id, the method and its params, and a session only when given", async () => {
    const f = serve(() => ({ result: {} }));
    const { send } = await connect(f.url, { onClose: keep });
    await send("Page.enable");
    await send("Page.navigate", { url: "about:blank" }, "S1");
    expect(JSON.stringify(f.got[0])).toBe(`{"id":1,"method":"Page.enable","params":{}}`);
    expect(f.got[1]).toEqual({ id: 2, method: "Page.navigate", params: { url: "about:blank" }, sessionId: "S1" });
  });

  test("a failed command says the method and Chrome's words, or the check's own", async () => {
    const f = serve(() => ({ error: { code: -32000, message: "Cannot navigate" } }));
    const plain = await connect(f.url, { onClose: keep });
    await expect(plain.send("Page.navigate")).rejects.toThrow("Page.navigate: Cannot navigate");
    const own = await connect(f.url, { onClose: keep, errorText: (method, message) => method + message });
    await expect(own.send("Page.navigate")).rejects.toThrow("Page.navigateCannot navigate");
  });

  test("with a time limit, a command with no answer fails, and the late answer is dropped", async () => {
    const f = serve(() => undefined);
    const { send } = await connect(f.url, { onClose: keep, timeoutMs: 50 });
    await expect(send("Page.enable")).rejects.toThrow("Page.enable: no answer in 0.05 s");
    await Bun.sleep(80);
    f.reply({ id: 1, result: {} }); // nothing waits for it now, so it reaches no one
  });

  test("without one, a command waits", async () => {
    const f = serve(() => undefined);
    const { send } = await connect(f.url, { onClose: keep });
    let settled = false;
    send("Page.enable").then(() => (settled = true), () => (settled = true));
    await Bun.sleep(150);
    expect(settled).toBe(false);
    f.reply({ id: 1, result: {} });
    await Bun.sleep(20);
    expect(settled).toBe(true);
  });
});

describe("events", () => {
  test("a message that isn't an answer goes to the event handlers, and an answer doesn't", async () => {
    const f = serve(() => ({ result: {} }));
    const { send, onEvent } = await connect(f.url, { onClose: keep });
    const seen: Message[] = [];
    onEvent((m) => seen.push(m));
    await send("Page.enable");
    f.reply({ method: "Runtime.exceptionThrown", params: { n: 1 }, sessionId: "S1" });
    await Bun.sleep(20);
    expect(seen).toEqual([{ method: "Runtime.exceptionThrown", params: { n: 1 }, sessionId: "S1" }]);
  });
});

describe("the connection", () => {
  test("closing it from the browser's side runs onClose", async () => {
    const f = serve(() => undefined);
    let closed = false;
    await connect(f.url, { onClose: () => (closed = true) });
    f.close();
    await Bun.sleep(50);
    expect(closed).toBe(true);
  });

  test("with rejectOpenError, a connection that can't open rejects", async () => {
    const f = serve(() => undefined);
    const url = f.url;
    f.stop();
    servers.length = 0;
    await expect(connect(url, { onClose: keep, rejectOpenError: true })).rejects.toBeDefined();
  });

  test("openPage asks for a new tab with PUT, then talks to that tab", async () => {
    const f = serve(() => ({ result: { ok: true } }));
    const { send } = await openPage(f.base, { onClose: keep });
    expect(f.got[0]).toEqual({ http: "PUT", path: "/json/new?about:blank" });
    expect(await send("Page.enable")).toEqual({ ok: true });
  });
});

describe("evaluate", () => {
  test("gives back the value, waiting on a promise", async () => {
    const f = serve((m) => ({ result: { result: { value: m.params.expression === "6*7" ? 42 : null } } }));
    const { send } = await connect(f.url, { onClose: keep });
    expect(await evaluate(send, "6*7")).toBe(42);
    expect(f.got[0].params).toEqual({ expression: "6*7", awaitPromise: true, returnByValue: true });
  });

  test("an exception in the page throws its description, its text, or the check's own words", async () => {
    const details = { text: "Uncaught", exception: { description: "TypeError: x is null" } };
    const f = serve((m) => ({ result: { exceptionDetails: m.params.expression === "plain" ? { text: "Uncaught" } : details } }));
    const { send } = await connect(f.url, { onClose: keep });
    await expect(evaluate(send, "x.y")).rejects.toThrow("TypeError: x is null");
    await expect(evaluate(send, "plain")).rejects.toThrow("Uncaught");
    await expect(evaluate(send, "x.y", (d) => JSON.stringify(d).slice(0, 20))).rejects.toThrow(JSON.stringify(details).slice(0, 20));
  });

  test("in a session, it's sent to that session", async () => {
    const f = serve(() => ({ result: { result: { value: 1 } } }));
    const { send } = await connect(f.url, { onClose: keep });
    await evaluate(send, "1", undefined, "S9");
    expect(f.got[0].sessionId).toBe("S9");
  });
});
