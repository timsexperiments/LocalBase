// The browser suite's checks, driven over the DevTools protocol. Everything waits on the
// page's state, never on a timer: `until` polls a condition and, if it never holds, fails
// loudly with a screenshot, the address and the console. There are no retries.
//
// Two parts: journeys (sign in, chat, Studio, failure) and a matrix of pages x widths x
// themes. E2E_PAGES, E2E_WIDTHS and E2E_THEMES narrow the matrix; E2E_PART picks
// "journeys" or "matrix".
const [cdpBase, base, control, out, axePath, wavPath] = process.argv.slice(2);
const { mkdirSync, writeFileSync } = await import("node:fs");
const captures = `${out}/captures`;
mkdirSync(captures, { recursive: true });
const list = (name: string) => process.env[name]?.split(",").filter(Boolean);
const part = process.env.E2E_PART;

const WIDTHS = (list("E2E_WIDTHS")?.map(Number) ?? [320, 360, 390, 430, 768, 1280]);
const THEMES = list("E2E_THEMES") ?? ["light", "dark"];
const HEIGHT = (w: number) => (w < 768 ? 800 : 900);
// [name, path, signed in?]. Keyboard, sheets and reduced motion run per page; overflow and
// axe run per cell.
const ALL_PAGES: [string, string, boolean][] = [
  ["signin", "/app/login", false],
  ["signin-used-link", "/app/login/once#token=used", false],
  ["chat", "/app/chat", true],
  ["welcome", "/app/welcome", true],
  ["welcome-choose", "/app/welcome/choose", true],
  ["studio", "/app/studio", true],
  ["image", "/app/studio/image", true],
  ["video", "/app/studio/video", true],
  ["speech", "/app/studio/speech", true],
  ["transcribe", "/app/studio/transcribe", true],
  ["decisions", "/app/studio/decisions", true],
  ["models", "/app/models", true],
  ["get", "/app/models/get", true],
  ["running", "/app/models/running", true],
  ["voices", "/app/voices", true],
  ["settings", "/app/settings", true],
  ["admin-people", "/app/admin?section=people", true],
  ["admin-access", "/app/admin?section=access", true],
  ["admin-keys", "/app/admin?section=keys", true],
  ["not-found", "/app/nope", true],
];
const PAGES = ALL_PAGES.filter(([n]) => !list("E2E_PAGES") || list("E2E_PAGES")!.includes(n));

// --- DevTools ---------------------------------------------------------------------------

const version = await (await fetch(`${cdpBase}/json/version`)).json();
const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((r, j) => { ws.addEventListener("open", r, { once: true }); ws.addEventListener("error", j, { once: true }); });
let nextId = 0;
const waiting = new Map<number, (m: any) => void>();
const listeners = new Map<string, (method: string, params: any) => void>();
ws.addEventListener("message", (e) => {
  const m = JSON.parse(String(e.data));
  if (m.id && waiting.has(m.id)) { waiting.get(m.id)!(m); waiting.delete(m.id); return; }
  if (m.sessionId && listeners.has(m.sessionId)) listeners.get(m.sessionId)!(m.method, m.params);
});
const send = (method: string, params: any = {}, sessionId?: string) => new Promise<any>((res, rej) => {
  const n = ++nextId;
  const timer = setTimeout(() => { waiting.delete(n); rej(new Error(`${method}: no answer in 30 s`)); }, 30_000);
  waiting.set(n, (m) => { clearTimeout(timer); m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result); });
  ws.send(JSON.stringify({ id: n, method, params, ...(sessionId ? { sessionId } : {}) }));
});

class Stuck extends Error {}
let selfTesting = false;

class Tab {
  session = ""; problems: string[] = []; ctx?: string; target = "";
  static async open(ctx?: string) {
    const t = new Tab();
    t.ctx = ctx;
    const { targetId } = await send("Target.createTarget", { url: "about:blank", ...(ctx ? { browserContextId: ctx } : {}) });
    t.target = targetId;
    t.session = (await send("Target.attachToTarget", { targetId, flatten: true })).sessionId;
    listeners.set(t.session, (method, p) => {
      if (method === "Runtime.exceptionThrown") t.problems.push(`exception: ${p.exceptionDetails.exception?.description ?? p.exceptionDetails.text}`.slice(0, 300));
      if (method === "Log.entryAdded" && p.entry.level === "error" && !/status of (4\d\d)/.test(p.entry.text)) t.problems.push(`console: ${p.entry.text}`.slice(0, 300));
      if (method === "Network.responseReceived" && p.response.status >= 500) t.problems.push(`HTTP ${p.response.status}: ${p.response.url.replace(base, "")}`);
      if (method === "Network.loadingFailed" && !p.canceled && p.type !== "Ping") t.problems.push(`load failed (${p.errorText}): ${p.type}`);
    });
    for (const d of ["Page", "Runtime", "Log", "Network"]) await t.cmd(`${d}.enable`);
    // `:focus` only matches in a focused page, and another tab may hold the focus.
    await t.cmd("Emulation.setFocusEmulationEnabled", { enabled: true });
    return t;
  }
  cmd(method: string, params: any = {}) { return send(method, params, this.session); }
  async ev(expression: string) {
    const r = await this.cmd("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(`in the page: ${r.exceptionDetails.exception?.description ?? r.exceptionDetails.text}`.slice(0, 400));
    return r.result.value;
  }
  async until(what: string, expression: string, ms = 15_000) {
    const deadline = Date.now() + ms;
    let last: any;
    for (;;) {
      try { last = await this.ev(expression); if (last) return last; } catch (e) { last = String(e); } // mid-navigation
      if (Date.now() > deadline) break;
      await Bun.sleep(40);
    }
    const shot = await this.capture(`stuck-${slug(what)}`);
    let url = "?"; try { url = await this.ev("location.href"); } catch {}
    throw new Stuck(`waited ${ms} ms for: ${what}\n  at ${url.replace(base, "")}; last value ${JSON.stringify(last)?.slice(0, 200)}\n  capture ${shot}\n  console: ${this.problems.slice(-3).join(" | ") || "clean"}`);
  }
  // Loaded, fonts in, htmx quiet, two frames drawn. Nothing here sleeps for a set time.
  async nav(path: string) {
    await this.cmd("Page.navigate", { url: path.startsWith("http") ? path : base + path });
    await this.settle();
  }
  async settle() {
    await this.until("the page to load", `document.readyState === "complete" && location.href !== "about:blank"`);
    await this.ev(`document.fonts.ready.then(() => true)`);
    await this.until("htmx to go quiet", `!document.querySelector(".htmx-request")`);
    await this.ev(`new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`);
  }
  async metrics(width: number, theme: string, reduced = false, touch = width < 1024) {
    await this.cmd("Emulation.setDeviceMetricsOverride", { width, height: HEIGHT(width), deviceScaleFactor: touch ? 2 : 1, mobile: touch });
    await this.cmd("Emulation.setTouchEmulationEnabled", { enabled: touch });
    await this.cmd("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: theme },
      { name: "prefers-reduced-motion", value: reduced ? "reduce" : "no-preference" }] });
  }
  async capture(name: string) {
    if (selfTesting) return "(self-test)";
    const file = `${captures}/${slug(name)}.png`;
    try {
      const { data } = await this.cmd("Page.captureScreenshot", { format: "png" });
      await Bun.write(file, Buffer.from(data, "base64"));
      return file.replace(`${out}/`, "");
    } catch (e) { return `(no capture: ${e})`; }
  }
  async key(key: string, shift = false) {
    const codes: Record<string, number> = { Tab: 9, Enter: 13, Escape: 27, " ": 32 };
    const common = { key, code: key === " " ? "Space" : key, windowsVirtualKeyCode: codes[key], modifiers: shift ? 8 : 0 };
    // Enter and Space only activate a control as a keyDown that carries text.
    const text = key === "Enter" ? "\r" : key === " " ? " " : undefined;
    await this.cmd("Input.dispatchKeyEvent", text ? { type: "keyDown", ...common, text, unmodifiedText: text } : { type: "rawKeyDown", ...common });
    await this.cmd("Input.dispatchKeyEvent", { type: "keyUp", ...common });
  }
  async close() { listeners.delete(this.session); await send("Target.closeTarget", { targetId: this.target }).catch(() => {}); }
}
const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 90);
const ctl = async (path: string) => {
  const r = await fetch(control + path, { method: "POST" });
  const text = await r.text();
  if (!r.ok) throw new Error(`control ${path}: ${r.status} ${text.slice(0, 200)}`);
  return text;
};
const models = await (await fetch(`${control}/models`)).json();

// --- findings ---------------------------------------------------------------------------

type Finding = { kind: string; page: string; width?: number; theme?: string; detail: string; capture?: string };
const findings: Finding[] = [];
const journeys: { name: string; ok: boolean; detail?: string; ms: number }[] = [];
const cells = new Map<string, { overflow: number; axe: number; other: number }>();
const cellKey = (p: string, w: number, t: string) => `${p}|${w}|${t}`;

async function journey(name: string, fn: () => Promise<void>) {
  const began = Date.now();
  try {
    await fn();
    journeys.push({ name, ok: true, ms: Date.now() - began });
    console.log(`ok    ${name} (${Date.now() - began} ms)`);
  } catch (e: any) {
    journeys.push({ name, ok: false, detail: String(e.message ?? e), ms: Date.now() - began });
    console.log(`FAIL  ${name}\n  ${String(e.message ?? e)}`);
  }
}
const need = (ok: any, what: string) => { if (!ok) throw new Error(what); };

// --- journeys ---------------------------------------------------------------------------

// A signed-in browser. The cookie is real: it comes from redeeming a first-run link.
const browser = await send("Target.createBrowserContext");
const main = await Tab.open(browser.browserContextId);
await main.metrics(390, "light");
const linkFromControl = async () => {
  const body = JSON.parse(await ctl("/link"));
  const url = body.data?.url ?? body.url;
  if (!url) throw new Error("no link in: " + JSON.stringify(body).replace(/token=[^"&]+/g, "token=<hidden>").slice(0, 200));
  return url as string;
};
let usedLink = "";

async function signIn() {
  await journey("before signing in, the app asks for a link and says how to get one", async () => {
    await main.nav("/app/chat");
    await main.until("the sign-in page", `document.querySelector(".welcome-title")?.textContent.includes("Sign in")`);
    need(await main.ev(`document.body.textContent.includes("local-base access login-link")`), "the sign-in page does not name the command");
  });
  await journey("a first-run link signs in, with nothing of the token left in the address", async () => {
    usedLink = await linkFromControl();
    await main.nav(usedLink);
    await main.until("the signed-in app", `location.pathname.startsWith("/app/") && !location.pathname.startsWith("/app/login") && !!document.querySelector("nav")`);
    need(!(await main.ev(`location.href.includes("token") || location.hash.length > 1`)), "the token stayed in the address bar");
    const cookies = (await main.cmd("Network.getCookies", { urls: [base] })).cookies;
    need(cookies.some((c: any) => c.httpOnly), "no HttpOnly session cookie: " + cookies.map((c: any) => c.name));
  });
  await journey("the same link does not work twice", async () => {
    const other = await send("Target.createBrowserContext");
    const t = await Tab.open(other.browserContextId);
    try {
      await t.metrics(390, "light");
      await t.nav(usedLink);
      await t.until("the link's refusal", `/run out/.test(document.querySelector(".welcome-title")?.textContent ?? "")`);
      const c = (await t.cmd("Network.getCookies", { urls: [base] })).cookies;
      need(!c.some((x: any) => x.httpOnly), "a used link set a session cookie");
    } finally { await t.close(); await send("Target.disposeBrowserContext", { browserContextId: other.browserContextId }); }
  });
}

const typeAndSend = (text: string) => `(() => { const i = document.querySelector("#chat-input"); i.value = ${JSON.stringify(text)}; i.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector("#composer").requestSubmit(); })()`;
const lastReply = `document.querySelector("[data-messages] article:last-child .reply")?.textContent ?? ""`;

async function firstChat() {
  await journey("a first chat gets its reply", async () => {
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await main.ev(typeAndSend("fake:reply The harbor is quiet today."));
    await main.until("the reply", `(${lastReply}).includes("The harbor is quiet today.")`);
    await main.until("the send button back", `!document.querySelector("[data-stop]") || document.querySelector("[data-stop]").hidden`);
  });
  await journey("a streaming reply can be stopped, keeping what it had", async () => {
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await main.ev(typeAndSend("fake:slow 400"));
    await main.until("the reply to start", `(${lastReply}).includes("w3 ")`);
    await main.ev(`document.querySelector("[data-stop]").click()`);
    await main.until("the reply to stop", `!document.querySelector("[data-stop]") || document.querySelector("[data-stop]").hidden`);
    const words = await main.ev(`(${lastReply}).trim().split(/\\s+/).length`);
    need(words >= 3 && words < 400, `after Stop the reply has ${words} words`);
  });
  await journey("markdown in a reply is not injected as HTML", async () => {
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await main.ev(typeAndSend(`fake:reply <img src=x onerror="window.__xss=1"> done`));
    await main.until("the reply", `(${lastReply}).includes("done")`);
    need(!(await main.ev(`window.__xss === 1 || !!document.querySelector("[data-messages] .reply img[src=x]")`)), "HTML from a reply ran in the page");
  });
}

async function studio() {
  const prompt = (text: string) => `(() => { const p = document.querySelector("#lab-prompt"); p.value = ${JSON.stringify(text)}; p.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector(".lab-dock button[type=submit]").click(); })()`;
  await journey("Image makes a picture a browser can decode", async () => {
    await main.nav("/app/studio/image");
    await main.until("the prompt", `!!document.querySelector("#lab-prompt")`);
    await main.ev(prompt("a paper boat"));
    await main.until("a decoded picture", `[...document.querySelectorAll("#lab-items img")].some((i) => i.complete && i.naturalWidth > 0)`, 30_000);
  });
  await journey("Speech makes audio with a player", async () => {
    await main.nav("/app/studio/speech");
    await main.until("the prompt", `!!document.querySelector("#lab-prompt")`);
    await main.ev(prompt("Good morning, this is a test."));
    await main.until("a player with audio", `!!document.querySelector("#lab-items [data-speech-player] audio[src]")`, 30_000);
  });
  await journey("Transcribe turns a file into text", async () => {
    await main.nav("/app/studio/transcribe");
    await main.until("the file input", `!!document.querySelector("[data-audio-input]")`);
    const { root } = await main.cmd("DOM.getDocument");
    const { nodeId } = await main.cmd("DOM.querySelector", { nodeId: root.nodeId, selector: "[data-audio-input]" });
    await main.cmd("DOM.setFileInputFiles", { nodeId, files: [wavPath] });
    await main.until("the transcript", `[...document.querySelectorAll("#lab-items [data-transcript]")].some((t) => t.textContent.includes("harbor"))`, 30_000);
  });
  await journey("Decisions answers a question", async () => {
    await main.nav("/app/studio/decisions");
    await main.until("the form", `!!document.querySelector("#decide-situation")`);
    await main.ev(`(() => { const a = document.getElementById("decide-situation"); a.value = "A customer asks for a refund"; a.dispatchEvent(new Event("input", { bubbles: true })); })()`);
    await main.until("option rows", `document.querySelectorAll("[data-option][data-part=name]").length >= 2`);
    await main.ev(`(() => { const o = document.querySelectorAll("[data-option][data-part=name]"); ["Refund", "Decline"].forEach((v, i) => { o[i].value = v; o[i].dispatchEvent(new Event("input", { bubbles: true })); }); const q = document.querySelector("#lab-prompt"); q.value = "What should we do?"; q.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector("#decide").requestSubmit(); })()`);
    await main.until("the answer", `(() => { const r = document.querySelector("[data-result]"); return r && !r.hidden && /Refund|Decline/.test(r.textContent); })()`, 30_000);
  });
}

const stripFree = (gib: number) => `document.querySelector("[data-strip-free]")?.textContent === "${gib} GiB free"`;

async function failures() {
  await journey("a page with no such route says so", async () => {
    await main.nav("/app/nope");
    await main.until("the not-found page", `/not|isn.t here|can.t find/i.test(document.querySelector("main")?.textContent ?? "")`);
  });
  await journey("too little memory: chat explains, offers a way on, and works once there is room", async () => {
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await ctl(`/unload?model=${models.chat}`).catch(() => {});
    await ctl("/host?available=11");
    // The refusal is what ends this wait.
    let refused = false;
    for (const end = Date.now() + 20_000; Date.now() < end && !refused;) {
      await main.ev(typeAndSend("fake:reply room now"));
      await main.until("an answer or a refusal", `(${lastReply}).includes("room now") || !!document.querySelector("[data-error-box] .font-semibold, [data-error-box] button")`, 15_000);
      refused = await main.ev(`/Not enough free memory/.test(document.querySelector("[data-error-box]")?.textContent ?? "")`);
      if (!refused) await ctl(`/unload?model=${models.chat}`).catch(() => {});
    }
    need(refused, "chat never showed the memory refusal");
    await main.capture("journey-memory-refusal");
    // Told there is too little memory, a person must not go on reading that there is plenty.
    await main.until("the gauge to show what is free", stripFree(11));
    // A screen reader is told the same, with the words apart.
    const { root } = await main.cmd("DOM.getDocument");
    const { nodeId } = await main.cmd("DOM.querySelector", { nodeId: root.nodeId, selector: "#memory-strip .strip" });
    const spoken = (await main.cmd("Accessibility.getPartialAXTree", { nodeId, fetchRelatives: false })).nodes[0]?.name?.value ?? "";
    need(/[a-z.,:;]\s+11 GiB free/.test(spoken), `the strip reads as: ${spoken}`);
    await ctl("/host?available=48");
    // What the refusal tells a person to do: free some memory and try again. It counts when
    // the reply comes, since the person's own words are on the page from the start. Refusals
    // go on for about 4.6 s, on purpose: recovery takes three normal samples at a 2 s poll.
    const freed = Date.now();
    let tries = 0;
    for (;;) {
      if (await main.ev(`(${lastReply}).includes("room now")`)) break;
      if (Date.now() - freed > 30_000) throw new Error(`no reply 30 s after memory came back, after ${tries} tries`);
      if (await main.ev(`(() => { const again = [...document.querySelectorAll("[data-error-box] button")].find((b) => /Try again/.test(b.textContent)); again?.click(); return !!again; })()`)) tries++;
      await Bun.sleep(40);
    }
    console.log(`memory back: ${tries} tries, the reply came after ${Date.now() - freed} ms`);
  });
  await journey("a backend killed mid-reply: the reply ends with a way on, and the next one works", async () => {
    await ctl("/host?available=48"); // whatever the journey before it got to
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await main.ev(typeAndSend("fake:slow 400"));
    await main.until("the reply to start", `(${lastReply}).includes("w3 ")`);
    const killed = JSON.parse(await ctl("/kill?name=llama-server")).killed;
    need(killed === 1, `killed ${killed} backends`);
    await main.until("the reply to end", `!document.querySelector("[data-stop]") || document.querySelector("[data-stop]").hidden`);
    await main.capture("journey-backend-killed");
    const shown = await main.ev(`document.querySelector("[data-messages] article:last-child")?.textContent + " " + (document.querySelector("[data-error-box]")?.textContent ?? "")`);
    need(/interrupt|stopp|failed|try again|didn.t finish|couldn.t|unavailable/i.test(shown), `nothing tells the person it failed: ${shown.slice(0, 200)}`);
    await main.nav("/app/chat?new=1");
    await main.until("the composer", `!!document.querySelector("#chat-input")`);
    await main.ev(typeAndSend("fake:reply back again"));
    await main.until("the next reply", `(${lastReply}).includes("back again")`, 30_000);
  });
  await journey("ending every session sends this browser back to sign-in, and a new link works", async () => {
    await ctl("/revoke");
    await main.nav("/app/chat");
    await main.until("the sign-in page", `document.querySelector(".welcome-title")?.textContent.includes("Sign in")`);
    await main.nav(await linkFromControl());
    await main.until("the signed-in app", `location.pathname.startsWith("/app/") && !location.pathname.startsWith("/app/login") && !!document.querySelector("nav")`);
  });
}

// --- the matrix -------------------------------------------------------------------------

const axeSource = await Bun.file(axePath).text();
const OVERFLOW = (w: number) => `(() => {
  const W = ${w}, se = document.scrollingElement;
  const scrolls = (el) => { for (let e = el.parentElement; e; e = e.parentElement) { const o = getComputedStyle(e).overflowX; if (o !== "visible" && e !== document.body && e !== document.documentElement) return true; } return false; };
  const r = { over: se.scrollWidth - W, client: document.documentElement.clientWidth, inner: innerWidth, wide: [] };
  if (r.over > 0 || r.client !== W) for (const el of document.querySelectorAll("body *")) {
    const b = el.getBoundingClientRect();
    if (b.width > 0 && b.right > W + 1 && getComputedStyle(el).visibility !== "hidden" && !scrolls(el)) {
      r.wide.push(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + (el.className && el.className.baseVal === undefined ? "." + String(el.className).trim().split(/\\s+/).slice(0, 2).join(".") : "") + " right=" + Math.round(b.right));
      if (r.wide.length >= 5) break;
    }
  }
  return r;
})()`;

// The width comparison must be against the width asked for. In mobile emulation innerWidth
// can grow with the content, so scrollWidth - innerWidth reads 0 on a page that overflows.
// This plants an overflow and requires the check to see it.
async function overflowSelfTest(t: Tab) {
  await t.metrics(390, "light");
  await t.nav("/app/login");
  const clean = await t.ev(OVERFLOW(390));
  await t.ev(`(() => { const d = document.createElement("div"); d.style.cssText = "width:1200px;height:4px"; document.body.append(d); })()`);
  await t.until("the planted element to lay out", `document.scrollingElement.scrollWidth > 390`);
  const planted = await t.ev(OVERFLOW(390));
  const oldWay = await t.ev(`document.documentElement.scrollWidth - innerWidth`);
  console.log(`overflow self-test: clean page ${clean.over}; planted ${planted.over} (innerWidth then ${planted.inner}, clientWidth ${planted.client}; the old comparison reads ${oldWay})`);
  if (planted.over <= 0) throw new Error(`the overflow check missed a planted 1200 px element: ${JSON.stringify(planted)}`);
  return { plantedOver: planted.over, oldComparison: oldWay, innerWidth: planted.inner };
}

// A check that cannot fail proves nothing, so each is shown a page that breaks it. What they
// report is taken back: only real pages produce findings. Everything is planted through
// script and CSSOM, which the page's content security policy allows.
async function checkTheChecks(t: Tab) {
  selfTesting = true;
  const before = findings.length;
  const proved: string[] = [];
  const login = "/app/login";
  const saw = (check: string, kind: string, detail: RegExp) => {
    if (!findings.slice(before).some((f) => f.kind === kind && detail.test(f.detail))) throw new Error(`the ${check} check missed a planted failure; it reported: ${JSON.stringify(findings.slice(before)).slice(0, 400)}`);
    proved.push(check);
    findings.length = before;
  };
  const plant = (html: string, then = "") => `(() => { document.body.insertAdjacentHTML("beforeend", ${JSON.stringify(html)}); const p = document.getElementById("planted"); ${then} })()`;
  await t.metrics(1280, "light", false, false);

  t.problems = [];
  await t.nav(login);
  await t.ev(`setTimeout(() => { throw new Error("planted"); }, 0); new Promise((r) => setTimeout(() => r(true), 20))`);
  if (!t.problems.some((p) => /planted/.test(p))) throw new Error("the page-error check missed a planted exception");
  proved.push("page errors");

  await t.ev(plant(`<img id="planted" width="20" height="20" src="data:,">`));
  if (!(await axeScan(t)).some((v: any) => v.id === "image-alt")) throw new Error("the axe check missed a planted image with no alt text");
  proved.push("axe");

  await keyboard(t, "self-test", login, 1280, ADD_CSS("#planted:focus{outline:none!important;box-shadow:none!important}") + ";" + plant(`<button id="planted">No ring</button>`));
  saw("focus visibility", "focus-not-visible", /button#planted/);
  await keyboard(t, "self-test", login, 1280, plant(`<button id="planted">Skipped</button><button id="after">After</button>`, `p.addEventListener("focus", () => document.getElementById("after").focus());`));
  saw("Tab reachability", "not-reachable", /button#planted/);
  await keyboard(t, "self-test", login, 1280, plant(`<button id="planted">Trap</button>`, `p.addEventListener("keydown", (e) => { if (e.key === "Tab") e.preventDefault(); });`));
  saw("keyboard trap", "keyboard-trap", /button#planted/);
  await keyboard(t, "self-test", login, 1280, plant(`<button id="planted">Away</button>`, `p.style.cssText = "position:fixed;left:-500px;top:0";`));
  saw("focus off screen", "focus-off-screen", /button#planted/);
  await keyboard(t, "self-test", login, 1280, plant(`<button id="planted">Under</button><div id="cover"></div>`, `p.style.cssText = "position:fixed;left:10px;top:10px"; document.getElementById("cover").style.cssText = "position:fixed;inset:0;z-index:99999";`));
  saw("focus covered", "focus-covered", /button#planted/);

  await sheets(t, "self-test", login, 1280, plant(`<button id="planted" data-open-sheet="dead">Dead</button>`));
  saw("sheet opens", "sheet", /Enter did not open it/);
  const loose = plant(`<button id="planted" data-open-sheet="loose">Loose</button><dialog id="sheet"><button>Inside</button></dialog>`, `p.addEventListener("click", () => { document.getElementById("sheet").show(); p.focus(); });`);
  await sheets(t, "self-test", login, 1280, loose);
  const looseFound = findings.slice(before).map((f) => f.detail).join(" | ");
  for (const [check, said] of [["focus moves into a sheet", /focus is not inside the sheet/], ["Tab stays in a sheet", /Tab leaves the open sheet/], ["Escape closes a sheet", /Escape did not close it/]] as const) {
    if (!said.test(looseFound)) throw new Error(`the ${check} check missed a planted failure; it reported: ${looseFound.slice(0, 400)}`);
    proved.push(check);
  }
  findings.length = before;
  await sheets(t, "self-test", login, 1280, plant(`<button id="planted" data-open-sheet="elsewhere">Elsewhere</button><button id="other">Other</button><dialog id="sheet"><button>Inside</button></dialog>`, `p.addEventListener("click", () => { document.getElementById("other").focus(); document.getElementById("sheet").showModal(); });`));
  saw("focus returns from a sheet", "sheet", /focus does not return to the opener/);

  await t.metrics(1280, "light", true, false);
  await reducedMotion(t, "self-test", login, 1280, ADD_CSS("@keyframes planted{to{transform:rotate(360deg)}}#planted{animation:planted 1s linear infinite;width:10px;height:10px}") + ";" + plant(`<div id="planted"></div>`));
  saw("reduced motion", "reduced-motion", /planted/);
  selfTesting = false;
  console.log(`self-tests: each caught a planted failure: ${proved.join(", ")}`);
  return proved;
}

async function axeScan(t: Tab) {
  await t.ev(`if (!window.axe) { ${axeSource}; true }`);
  return await t.ev(`axe.run(document, { runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"] }, resultTypes: ["violations"] }).then((r) => r.violations.map((v) => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.length, targets: v.nodes.slice(0, 3).map((n) => n.target.join(" ")) })))`);
}

async function matrix(signedIn: Tab, signedOut: Tab) {
  for (const [name, path, auth] of PAGES) {
    const t = auth ? signedIn : signedOut;
    for (const width of WIDTHS) for (const theme of THEMES) {
      await t.metrics(width, theme);
      t.problems = [];
      await t.nav(path);
      // A redirect to sign-in would pass every check on the wrong page.
      if (auth && (await t.ev(`location.pathname.startsWith("/app/login") || !!document.querySelector(".welcome-title")?.textContent.includes("Sign in")`))) throw new Error(`${path} sent a signed-in browser to sign-in`);
      const cell = { overflow: 0, axe: 0, other: 0 };
      cells.set(cellKey(name, width, theme), cell);
      const o = await t.ev(OVERFLOW(width));
      if (o.over > 0 || o.client !== width) {
        cell.overflow = Math.max(o.over, 1);
        findings.push({ kind: "overflow", page: name, width, theme, detail: `${o.over}px wider than ${width} (clientWidth ${o.client}, innerWidth ${o.inner}): ${o.wide.join("; ") || "no single element found"}`, capture: await t.capture(`overflow-${name}-${width}-${theme}`) });
      }
      const violations = await axeScan(t);
      if (violations.length) {
        cell.axe = violations.reduce((n: number, v: any) => n + v.nodes, 0);
        const shot = await t.capture(`axe-${name}-${width}-${theme}`);
        for (const v of violations) findings.push({ kind: `axe:${v.id}`, page: name, width, theme, detail: `${v.impact}: ${v.help} (${v.nodes} nodes): ${v.targets.join(", ")}`, capture: shot });
      }
      const expected = (p: string) => /HTTP 5\d\d/.test(p) || /exception/.test(p);
      for (const p of t.problems.filter(expected)) { cell.other++; findings.push({ kind: "page-error", page: name, width, theme, detail: p, capture: await t.capture(`error-${name}-${width}-${theme}`) }); }
    }
    console.log(`matrix ${name}: ${WIDTHS.length * THEMES.length} cells`);
    await perPage(t, name, path);
  }
}

// Per page, at a phone and a desktop width: Tab reaches everything, focus shows and stays
// on screen, sheets work from the keyboard, and reduced motion is honored.
async function perPage(t: Tab, name: string, path: string) {
  for (const width of WIDTHS.filter((w) => w === 390 || w === 1280)) {
    await t.metrics(width, "light", false, false);
    await keyboard(t, name, path, width);
    await sheets(t, name, path, width);
    await t.metrics(width, "light", true);
    await reducedMotion(t, name, path, width);
  }
}

// :disabled, not [disabled]: a control in a disabled fieldset has no attribute of its own.
const FOCUSABLE = `a[href],button:not(:disabled),input:not(:disabled):not([type=hidden]),select:not(:disabled),textarea:not(:disabled),summary,[tabindex]:not([tabindex="-1"])`;
// checkVisibility also sees what a closed <details> holds, which keeps its size and its computed
// visibility and still cannot take focus.
const SEEN = `(el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0 && el.checkVisibility({ visibilityProperty: true }) && !el.closest("[hidden],[inert],dialog:not([open])"); }`;

// How a finding names a control: what a person would call it, and where a developer finds it.
const NAME = `(e) => { const said = (e.getAttribute("aria-label") || e.textContent || e.getAttribute("title") || e.getAttribute("placeholder") || e.getAttribute("name") || "").trim().replace(/\\s+/g, " ").slice(0, 30); return e.tagName.toLowerCase() + (e.id ? "#" + e.id : "") + (e.type === "radio" || e.type === "checkbox" ? "[" + e.type + "]" : "") + " '" + said + "'"; }`;

const ADD_CSS = (css: string) => `(() => { const s = new CSSStyleSheet(); s.replaceSync(${JSON.stringify(css)}); document.adoptedStyleSheets = [...document.adoptedStyleSheets, s]; })()`;

// Where Tab should stop. A radio group is one stop, the checked radio or else the first; the
// arrow keys move inside it. A tabindex of -1 is out of the Tab order on purpose.
const TAB_STOPS = `(() => {
  const seen = ${SEEN};
  const stops = [...document.querySelectorAll(${JSON.stringify(FOCUSABLE)})].filter(seen).filter((e) => e.tabIndex >= 0).filter((e) => {
    if (e.type !== "radio" || !e.name) return true;
    const group = [...document.querySelectorAll("input[type=radio]")].filter((r) => r.name === e.name && r.form === e.form && seen(r));
    return (group.find((r) => r.checked) ?? group[0]) === e;
  });
  return stops.map((e, i) => { e.dataset.e2eId = "e" + i; return "e" + i; });
})()`;

// Whether focus shows: what a focused control looks like against the same control blurred,
// on itself, its before and after, the boxes around it (a composer shows focus on its frame)
// and the element after it (a hidden checkbox styles its neighbour).
const FOCUS_SHOWS = `(() => {
  const el = document.activeElement;
  const one = (c) => [c.outlineStyle !== "none" && parseFloat(c.outlineWidth) > 0 ? c.outlineStyle + c.outlineWidth + c.outlineColor : "", c.boxShadow, c.borderTopColor, c.backgroundColor, c.textDecorationLine].join("|");
  const look = (e) => one(getComputedStyle(e)) + one(getComputedStyle(e, "::before")) + one(getComputedStyle(e, "::after"));
  const chain = el.nextElementSibling ? [el.nextElementSibling] : [];
  for (let e = el, i = 0; e && e !== document.body && i < 4; e = e.parentElement, i++) chain.push(e);
  const focused = chain.map(look);
  el.blur();
  const blurred = chain.map(look);
  el.focus({ preventScroll: true });
  return focused.some((v, i) => v !== blurred[i]);
})()`;

async function keyboard(t: Tab, name: string, path: string, width: number, prepare?: string) {
  t.problems = [];
  await t.nav(path);
  // What is read is where a control ends up, not where a transition or a smooth scroll has got to.
  await t.ev(ADD_CSS("*,*::before,*::after{transition:none!important;animation:none!important;scroll-behavior:auto!important}"));
  if (prepare) await t.ev(prepare);
  const expected: string[] = await t.ev(TAB_STOPS);
  const reached = new Set<string>();
  const bad = new Map<string, string>();
  // Taken while the control has focus: by the end of the walk the page has scrolled away.
  const note = async (detail: string) => { if (!bad.has(detail) && bad.size < 8) bad.set(detail, await t.capture(`keyboard-${name}-${width}-${bad.size}`)); };
  let firstStop = "", repeats = 0, last = "", edges = 0;
  // The page may have put focus mid-way, so a walk goes off the end once and comes round.
  const limit = Math.min(expected.length * 3 + 16, 150);
  for (let i = 0; i < limit; i++) {
    await t.key("Tab");
    const s = await t.ev(`(() => {
      const el = document.activeElement;
      if (!el || el === document.body) return { body: true };
      const r = el.getBoundingClientRect();
      const on = r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth;
      const top = document.elementFromPoint(Math.min(Math.max(r.left + r.width / 2, 0), innerWidth - 1), Math.min(Math.max(r.top + r.height / 2, 0), innerHeight - 1));
      const covered = on && top && !(el === top || el.contains(top) || top.contains(el) || top.closest("label")?.contains(el));
      const media = el.matches("audio,video");
      return { id: el.dataset.e2eId ?? "", name: (${NAME})(el), on, media, ring: media || ${FOCUS_SHOWS}, covered: covered ? top.tagName.toLowerCase() + (top.className && top.className.baseVal === undefined ? "." + String(top.className).trim().split(/\\s+/)[0] : "") : "", sr: el.matches(".sr-only") };
    })()`);
    if (s.body) { if (++edges >= 2) break; continue; }
    const key = s.id || s.name;
    // Back at the start. A page with one stop comes straight back to it, and that is no trap.
    if (key === firstStop && reached.size >= Math.min(2, expected.length)) break;
    if (!firstStop) firstStop = key;
    repeats = key === last ? repeats + 1 : 0;
    last = key;
    if (s.id) reached.add(s.id);
    // A native player keeps focus on itself while Tab walks its own buttons.
    if (repeats >= (s.media ? 12 : 2)) { await note(`trap: Tab stays on ${s.name}`); break; }
    if (!s.on && !s.sr) await note(`off screen when focused: ${s.name}`);
    if (!s.ring && !s.sr) await note(`no visible focus: ${s.name}`);
    if (s.covered && !s.sr) await note(`focus hidden under ${s.covered}: ${s.name}`);
  }
  const missed = expected.filter((e) => !reached.has(e));
  if (missed.length) {
    const names = await t.ev(`${JSON.stringify(missed.slice(0, 5))}.map((id) => { const e = document.querySelector('[data-e2e-id="' + id + '"]'); return e ? (${NAME})(e) : id; })`);
    await note(`${missed.length} of ${expected.length} controls not reached by Tab, such as ${names.join(", ")}`);
  }
  const kindOf = (d: string) => (d.startsWith("trap") ? "keyboard-trap" : d.startsWith("off screen") ? "focus-off-screen" : d.startsWith("no visible") ? "focus-not-visible" : d.startsWith("focus hidden") ? "focus-covered" : "not-reachable");
  for (const [detail, capture] of bad) findings.push({ kind: kindOf(detail), page: name, width, detail, capture });
}

const OPENERS = `[data-open-sheet],[data-open-picker],[data-open-drawer],[data-open-generation]`;
async function sheets(t: Tab, name: string, path: string, width: number, prepare?: string) {
  await t.nav(path);
  if (prepare) await t.ev(prepare);
  const count = await t.ev(`(() => { const seen = ${SEEN}; return [...document.querySelectorAll(${JSON.stringify(OPENERS)})].filter(seen).length; })()`);
  for (let i = 0; i < Math.min(count, 6); i++) {
    await t.nav(path);
    if (prepare) await t.ev(prepare);
    const opener = await t.ev(`(() => { const seen = ${SEEN}; const o = [...document.querySelectorAll(${JSON.stringify(OPENERS)})].filter(seen)[${i}]; if (!o) return ""; o.dataset.e2eOpener = "1"; o.focus(); return (${NAME})(o); })()`);
    if (!opener) continue;
    const label = `${name} ${width} opener ${opener}`;
    const finding = async (detail: string) => findings.push({ kind: "sheet", page: name, width, detail: `${opener}: ${detail}`, capture: await t.capture(`sheet-${name}-${width}-${i}`) });
    await t.key("Enter");
    let opened = false;
    try { await t.until(`${label} to open`, `!!document.querySelector("dialog[open]")`, 2500); opened = true; } catch { /* a drawer that is no dialog on this width */ }
    if (!opened) { if (await t.ev(`!!document.querySelector("[aria-expanded=true]")`)) continue; await finding("Enter did not open it"); continue; }
    if (!(await t.ev(`!!document.activeElement?.closest("dialog[open]") || document.activeElement?.matches("dialog[open]")`))) await finding("focus is not inside the sheet when it opens");
    let contained = true;
    for (let k = 0; k < 12 && contained; k++) {
      await t.key("Tab");
      contained = await t.ev(`!!document.activeElement?.closest("dialog[open]") || document.activeElement === document.body`);
    }
    if (!contained) await finding("Tab leaves the open sheet");
    await t.key("Escape");
    try { await t.until(`${label} to close on Escape`, `!document.querySelector("dialog[open]")`, 2500); } catch { await finding("Escape did not close it"); continue; }
    if (!(await t.ev(`document.activeElement?.dataset?.e2eOpener === "1"`))) await finding(`focus does not return to the opener when it closes (it is on ${await t.ev(`document.activeElement?.tagName.toLowerCase()`)})`);
  }
}

async function reducedMotion(t: Tab, name: string, path: string, width: number, prepare?: string) {
  await t.nav(path);
  if (prepare) await t.ev(prepare);
  const moving = await t.ev(`document.getAnimations().filter((a) => a.playState === "running" && (a.effect?.getComputedTiming().iterations === Infinity || a.effect?.getComputedTiming().duration > 200)).map((a) => (a.animationName ?? a.transitionProperty ?? "animation") + " on " + (a.effect?.target?.tagName?.toLowerCase() ?? "?") + (a.effect?.target?.className && a.effect.target.className.baseVal === undefined ? "." + String(a.effect.target.className).trim().split(/\\s+/)[0] : "")).slice(0, 5)`);
  if (moving.length) findings.push({ kind: "reduced-motion", page: name, width, detail: `still animating with reduced motion asked for: ${moving.join(", ")}`, capture: await t.capture(`motion-${name}-${width}`) });
}

// --- the report -------------------------------------------------------------------------

function report(selfTest: any, wallMs: number) {
  const byKind = new Map<string, number>();
  for (const f of findings) byKind.set(f.kind, (byKind.get(f.kind) ?? 0) + 1);
  const failed = journeys.filter((j) => !j.ok);
  let md = `# Browser suite: ${failed.length || findings.length ? "FINDINGS" : "PASS"}\n\n`;
  md += `Headless ${version.Browser}, fake backends, ${(wallMs / 1000).toFixed(0)} s. ${journeys.length - failed.length} of ${journeys.length} journeys pass; ${findings.length} ${findings.length === 1 ? "finding" : "findings"} across ${PAGES.length} pages x ${WIDTHS.length} widths x ${THEMES.length} themes (${cells.size} cells).\n\n`;
  md += `## Journeys\n\n| journey | result | ms |\n|---|---|---:|\n`;
  for (const j of journeys) md += `| ${j.name} | ${j.ok ? "pass" : "**FAIL**"} | ${j.ms} |\n`;
  for (const j of failed) md += `\n**${j.name}**\n\n\`\`\`\n${j.detail}\n\`\`\`\n`;
  if (cells.size) {
    md += `\n## Matrix\n\nEach cell is light / dark: \`.\` is clean, \`O\` overflows, \`A\` has axe violations, \`E\` has a page error.\n\n| page | ${WIDTHS.join(" | ")} |\n|---|${WIDTHS.map(() => "---").join("|")}|\n`;
    for (const [name] of PAGES) {
      const row = WIDTHS.map((w) => THEMES.map((th) => {
        const c = cells.get(cellKey(name, w, th));
        if (!c) return "-";
        return (c.overflow ? "O" : "") + (c.axe ? "A" : "") + (c.other ? "E" : "") || ".";
      }).join(" / "));
      md += `| ${name} | ${row.join(" | ")} |\n`;
    }
    md += `\nOverflow compares \`scrollWidth\` with the width asked for, and \`clientWidth\` with the same. \`innerWidth\` can grow with the content in mobile emulation; a self-test plants a 1200 px element and requires the check to see it (${selfTest ? `it did: ${selfTest.plantedOver} px, when \`scrollWidth - innerWidth\` read ${selfTest.oldComparison}` : "not run"}).\n`;
    md += `\nPer cell: overflow, axe (WCAG 2.0, 2.1 and 2.2 A/AA) and page errors. Per page at 390 and 1280: Tab reaches every control (a radio group is one stop), focus changes how the control or a box around it looks, is on screen and not covered, no trap; sheets open with Enter, hold focus, close with Escape and return focus; reduced motion stops running animation.\n`;
    if (selfTest?.proved) md += `\nEach of these was shown a page that breaks it, and caught it: ${selfTest.proved.join(", ")}.\n`;
  }
  if (byKind.size) {
    md += `\n## Findings by kind\n\n| kind | count |\n|---|---:|\n`;
    for (const [k, n] of [...byKind].sort((a, b) => b[1] - a[1])) md += `| ${k} | ${n} |\n`;
    md += `\n## Findings\n\nGrouped by kind, page and detail; \`cells\` shows where. Captures are under \`captures/\`.\n`;
    const groups = new Map<string, { f: Finding; where: string[] }>();
    for (const f of findings) {
      const k = `${f.kind}|${f.page}|${f.detail}`;
      const g = groups.get(k) ?? { f, where: [] };
      g.where.push([f.width, f.theme].filter(Boolean).join(" "));
      groups.set(k, g);
    }
    const sorted = [...groups.values()].sort((a, b) => a.f.kind.localeCompare(b.f.kind) || a.f.page.localeCompare(b.f.page));
    for (const g of sorted.slice(0, 120)) md += `- **${g.f.kind}**, ${g.f.page}, cells ${g.where.join(", ") || "-"}: ${g.f.detail}${g.f.capture ? ` ([capture](${g.f.capture}))` : ""}\n`;
    if (sorted.length > 120) md += `- and ${sorted.length - 120} more; see findings.json\n`;
  }
  writeFileSync(`${out}/result.md`, md);
  writeFileSync(`${out}/findings.json`, JSON.stringify({ journeys, findings, cells: Object.fromEntries(cells), widths: WIDTHS, themes: THEMES, pages: PAGES.map(([n, p]) => ({ name: n, path: p })), overflowSelfTest: selfTest }, null, 1));
  console.log(md.split("## Findings\n")[0]);
  console.log(`findings ${findings.length}, failed journeys ${failed.length}`);
  return failed.length || findings.length ? 1 : 0;
}

const began = Date.now();
let selfTest: any = null;
let code = 2;
try {
  await signIn();
  // Everything else needs a signed-in browser, so failing here would only repeat itself.
  if (journeys.some((j) => !j.ok && /first-run link signs in/.test(j.name))) throw new Error("sign-in failed, so nothing else can run");
  if (part !== "matrix") {
    await firstChat();
    await studio();
    await failures();
  }
  if (part !== "journeys") {
    const outside = await send("Target.createBrowserContext");
    const signedOut = await Tab.open(outside.browserContextId);
    selfTest = { ...(await overflowSelfTest(signedOut)), proved: await checkTheChecks(signedOut) };
    await matrix(main, signedOut);
  }
  code = report(selfTest, Date.now() - began);
} catch (e: any) {
  console.error(`the suite stopped: ${e.message ?? e}`);
  try { code = Math.max(report(selfTest, Date.now() - began), 2); } catch { code = 2; }
}
ws.close();
process.exit(code);
