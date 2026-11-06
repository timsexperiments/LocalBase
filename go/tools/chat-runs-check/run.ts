// Drives headless Chrome for tools/chat-runs-check.sh: detached chat runs
// end to end (leave and return, the pill, Stop, the draft, a dropped
// connection). Exits 1 when any check fails.
const [cdpBase, base] = process.argv.slice(2);
// This script leaves its gateway changed, and on one it has already used it fails in ways that
// say nothing about the page. So: once per gateway, known by when it started.
{
  const marker = new URL("../../.cache/chat-runs-check.gateway", import.meta.url).pathname;
  const health = await (await fetch(base + "/health")).json();
  if (typeof health.uptimeSeconds === "number") {
    const started = Date.now() / 1000 - health.uptimeSeconds;
    const last = (await Bun.file(marker).text().catch(() => "")).trim().split(" ");
    if (last[0] === base && Math.abs(Number(last[1]) - started) < 5) {
      console.error("this check needs a fresh gateway: restart it (this run already used the one at " + base + ")");
      process.exit(4);
    }
    await Bun.write(marker, base + " " + started + "\n");
  }
}
const target = await (await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
// With its browser gone nothing here can finish: stop rather than wait on it.
ws.addEventListener("close", () => process.exit(2));
let id = 0; const waiting = new Map();
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.id && waiting.has(m.id)) { waiting.get(m.id)(m); waiting.delete(m.id); } });
const send = (method, params = {}) => new Promise((res, rej) => { if (method === "Emulation.setDeviceMetricsOverride") askedWidth = params.width; const n = ++id; waiting.set(n, (m) => m.error ? rej(new Error(method + m.error.message)) : res(m.result)); ws.send(JSON.stringify({ id: n, method, params })); });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// Whether the page fits the width last asked for. Not innerWidth: mobile emulation grows it
// to fit wide content, so scrollWidth - innerWidth stays 0 on a page that overflows.
let askedWidth = 0;
const pageFits = () => `Math.max(document.documentElement.scrollWidth, innerWidth) <= ${askedWidth}`;
const ev = async (expression) => { const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }); if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400)); return r.result.value; };
const nav = async (path) => { await send("Page.navigate", { url: base + path }); await sleep(1500); };
const runsNow = () => ev(`fetch("/app/api/_localbase/runs",{headers:{"x-localbase-ui":"1"}}).then(r=>r.json()).then(b=>b.data.map(r=>r.status+":"+r.label))`);
let fails = 0; const check = (ok, msg) => { console.log((ok ? "ok   " : "FAIL ") + msg); if (!ok) fails++; };
await send("Page.enable");
// A test tab can go hidden behind others, and a hidden page rightly stops polling and taking focus, which
// then looks like a bug in the page. Focus emulation keeps it active for the whole run.
await send("Emulation.setFocusEmulationEnabled", { enabled: true });
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });

await nav("/app/chat");
await ev(`fetch("/app/api/_localbase/runs",{headers:{"x-localbase-ui":"1"}}).then(r=>r.json()).then(b=>Promise.all(b.data.map(r=>fetch("/app/api/_localbase/runs/"+r.id,{method:"DELETE",headers:{"x-localbase-ui":"1"}}))))`);
// 1. Leave mid-reply, come back, get the whole reply.
await nav("/app/chat");
await ev(`(() => { const i = document.querySelector("#chat-input"); i.value = "fake:slow 120"; i.dispatchEvent(new Event("input")); document.querySelector("#composer").requestSubmit(); })()`);
await sleep(1500);
const conv = await ev(`new URL(location.href).searchParams.get("conversation")`);
check((await runsNow()).some((s) => s.startsWith("in_progress")), `a run is in progress: ${await runsNow()}`);
await nav("/app/models");
await sleep(800);
const pill = await ev(`(() => { const p = document.querySelector("[data-run-pill=dock]"); return { hidden: p.hidden, href: p.getAttribute("href"), text: p.textContent.trim() }; })()`);
check(!pill.hidden && pill.href.includes(conv), `pill on Models: ${JSON.stringify(pill)}`);
await sleep(2000);
await nav(pill.href);
for (let i = 0; i < 40 && !(await ev(`document.querySelector("[data-messages]").textContent.includes("w120")`)); i++) await sleep(250);
const text = await ev(`document.querySelector("[data-messages] article:last-child .reply").textContent`);
const words = text.trim().split(/\s+/);
check(words.length === 120 && words[0] === "w1" && words[119] === "w120", `full reply after returning (${words.length} words, no gaps or repeats: ${new Set(words).size === words.length})`);
await sleep(500);
check((await runsNow()).length === 0, `run dismissed after collecting: ${await runsNow()}`);
const pillHere = await ev(`document.querySelector("[data-run-pill=dock]").hidden`);
check(pillHere, "no pill once finished");

// 2. Stop cancels on the gateway and keeps the partial text.
await ev(`(() => { const i = document.querySelector("#chat-input"); i.value = "fake:slow 200"; i.dispatchEvent(new Event("input")); document.querySelector("#composer").requestSubmit(); })()`);
await sleep(1500);
await ev(`document.querySelector("[data-stop]").click()`);
await sleep(1200);
const after = await ev(`(() => { const a = document.querySelector("[data-messages] article:last-child"); return { text: a.querySelector(".reply").textContent.trim().split(/\\s+/).length, badge: a.textContent.includes("Interrupted") }; })()`);
check(after.text > 3 && after.text < 200 && after.badge, `Stop keeps partial text and marks it: ${JSON.stringify(after)}`);
check((await runsNow()).length === 0, `stopped run gone: ${await runsNow()}`);

// 3. Draft survives leaving the page.
await ev(`(() => { const i = document.querySelector("#chat-input"); i.value = "half a thought"; i.dispatchEvent(new Event("input")); })()`);
await nav("/app/studio");
await send("Page.navigate", { url: base + "/app/chat?conversation=" + conv }); await sleep(1500);
check((await ev(`document.querySelector("#chat-input").value`)) === "half a thought", "draft restored");
check((await ev(`document.querySelectorAll("[data-messages] article").length`)) >= 4, "conversation kept in this tab without saved history");
// 4. A dropped connection resumes from the last event: nothing lost or repeated.
await send("Network.enable");
await ev(`(() => { const i = document.querySelector("#chat-input"); i.value = "fake:slow 100"; i.dispatchEvent(new Event("input")); document.querySelector("#composer").requestSubmit(); })()`);
await sleep(1200);
await send("Network.emulateNetworkConditions", { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
await sleep(2000);
await send("Network.emulateNetworkConditions", { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
for (let i = 0; i < 60 && !(await ev(`document.querySelector("[data-messages] article:last-child").textContent.includes("w100")`)); i++) await sleep(250);
const w4 = (await ev(`document.querySelector("[data-messages] article:last-child .reply").textContent`)).trim().split(/\s+/);
check(w4.length === 100 && new Set(w4).size === 100 && w4[99] === "w100", `resumed after a dropped connection (${w4.length} words, unique ${new Set(w4).size})`);
// 5. Markdown with real newlines renders as blocks, and nothing widens the
// page at 360 px (long lines wrap; code and tables scroll inside themselves).
await send("Emulation.setDeviceMetricsOverride", { width: 360, height: 740, deviceScaleFactor: 2, mobile: true });
await nav("/app/chat");
const md = "fake:reply Intro:\n\n- one\n- two\n\n```js\nconst value = someFunction(withAVeryLongArgumentName, anotherArgumentThatIsLong, yetAnotherOne);\n```\n\n| column one | column two | column three | column four |\n|---|---|---|---|\n| 1 | 2 | 3 | 4 |\n\nSee `code` and [a link](https://example.com). " + "x".repeat(160);
await ev(`(() => { const i = document.querySelector("#chat-input"); i.value = ${JSON.stringify(md)}; i.dispatchEvent(new Event("input")); document.querySelector("#composer").requestSubmit(); })()`);
for (let i = 0; i < 40 && !(await ev(`!!document.querySelector("[data-messages] article:last-child .reply a")`)); i++) await sleep(250);
const r5 = await ev(`(() => { const a = document.querySelector("[data-messages] article:last-child .reply"); const w = document.documentElement.clientWidth;
  return { li: a.querySelectorAll("ul > li").length, pre: !!a.querySelector("pre"), table: !!a.querySelector("table"), code: !!a.querySelector(":not(pre) > code"), link: a.querySelector("a")?.getAttribute("href"),
    page: document.scrollingElement.scrollWidth - w, reply: Math.round(a.getBoundingClientRect().right - w), log: document.querySelector("#chat-log").scrollWidth - document.querySelector("#chat-log").clientWidth }; })()`);
check(r5.li === 2 && r5.pre && r5.table && r5.code && r5.link === "https://example.com", `markdown blocks render: ${JSON.stringify(r5)}`);
check(r5.page <= 0 && r5.reply <= 0 && r5.log <= 0, `nothing overflows at 360 px: page ${r5.page}, reply ${r5.reply}, log ${r5.log}`);
// 6. Running now: quiet while nothing changes, one announcement on a change.
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/models/running");
// An earlier request may still be finishing: wait until it has, not for a fixed time. Then start
// again from that steady state, and watch two polls with no change.
for (let i = 0; i < 60 && (await ev(`document.querySelector("#running-live .pill")?.textContent.trim()`)) !== "Idle"; i++) {
  await sleep(500);
  await nav("/app/models/running");
}
await nav("/app/models/running");
await sleep(7000);
const quiet = await ev(`document.getElementById("running-status").textContent`);
check(quiet === "", `no announcement while nothing changes: ${JSON.stringify(quiet)}`);
const idle = await ev(`document.querySelector("#running-live .pill")?.textContent.trim()`);
check(idle === "Idle", `the loaded model shows Idle: ${idle}`);
// Start a slow reply without waiting for it.
await ev(`void fetch("/app/api/v1/chat/completions", { method: "POST", headers: { "content-type": "application/json", "x-localbase-ui": "1" }, body: JSON.stringify({ model: "qwen3.5-4b-q4_k_m", stream: true, messages: [{ role: "user", content: "fake:slow 160" }] }) })`);
for (let i = 0; i < 20 && !(await ev(`document.getElementById("running-status").textContent`)); i++) await sleep(500);
const busy = await ev(`({ said: document.getElementById("running-status").textContent, pill: document.querySelector("#running-live .pill")?.textContent.trim() })`);
check(/is busy\.$/.test(busy.said) && busy.pill.startsWith("Busy"), `announces the change once: ${JSON.stringify(busy)}`);
// 7. Lab (needs the fake as sd-server too, LOCALBASE_SD_SERVER): a result
// keeps going while the person is away and is in the tray on return; Stop
// and Remove work; the prompt is docked above the tab bar, and above the
// keyboard while typing.
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await send("Emulation.setTouchEmulationEnabled", { enabled: true });
await nav("/app/studio/image");
await ev(`fetch("/app/api/_localbase/runs",{headers:{"x-localbase-ui":"1"}}).then(r=>r.json()).then(b=>Promise.all(b.data.map(r=>fetch("/app/api/_localbase/runs/"+r.id,{method:"DELETE",headers:{"x-localbase-ui":"1"}}))))`);
await nav("/app/studio/image");
if (!(await ev(`!!document.querySelector("#lab-prompt")`))) console.log("skip lab (no image model on this gateway)");
else {
  // At 360x740 the docked prompt stays small, and an example fills it.
  await send("Emulation.setDeviceMetricsOverride", { width: 360, height: 740, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/image");
  const h = await ev(`Math.round(document.querySelector(".lab-dock").getBoundingClientRect().height)`);
  check(h <= 0.2 * 740, `the docked prompt is at most a fifth of the screen at 360x740 (${h}px)`);
  await ev(`document.querySelector("[data-lab-empty] [data-fill]").click()`);
  const filled = await ev(`({ value: document.querySelector("#lab-prompt").value, focused: document.activeElement?.id })`);
  check(filled.value.startsWith("A paper boat") && filled.focused === "lab-prompt", `an example fills the prompt: ${JSON.stringify(filled)}`);
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/image");
  const dock = await ev(`(() => { const d = document.querySelector(".lab-dock").getBoundingClientRect(); const t = document.querySelector("nav.tabbar").getBoundingClientRect(); return { gap: Math.round(t.top - d.bottom) }; })()`);
  check(dock.gap === 0, `the prompt sits right above the tab bar (gap ${dock.gap})`);
  await ev(`(() => { document.querySelector("#lab-prompt").value = "a paper boat fake:slow-image"; document.querySelector(".lab-dock button[type=submit]").click(); })()`);
  for (let i = 0; i < 20 && !(await ev(`!!document.querySelector("#lab-items > li")`)); i++) await sleep(150);
  const started = await ev(`document.querySelector("#lab-items > li")?.textContent.replace(/\\s+/g, " ").trim()`);
  check(/paper boat/.test(started ?? ""), `a tray item appears with its caption: ${started}`);
  await nav("/app/models");
  await sleep(2000);
  await nav("/app/studio");
  check(await ev(`!!document.querySelector(".studio-recent a[href^='/app/studio/image']")`), "the landing's Recent shows it");
  check(await ev(`document.querySelector(".studio-recent img")?.naturalWidth > 0`), "a real picture decodes");
  // Recent says what each result is, in words, and leads to that result (DESIGN §27).
  const tile = await ev(`(() => { const a = document.querySelector(".studio-recent a"), cap = a.querySelector(".studio-tile-cap"), meta = a.querySelector(".studio-tile-meta"); return { cap: cap.textContent, meta: meta.textContent, seen: cap.getBoundingClientRect().height > 0 && meta.getBoundingClientRect().height > 0, tall: a.getBoundingClientRect().height, href: a.getAttribute("href"), over: document.documentElement.scrollWidth > document.documentElement.clientWidth }; })()`);
  check(/paper boat/.test(tile.cap) && /^Image · /.test(tile.meta) && tile.seen && tile.tall >= 44 && /^\/app\/studio\/image#run-/.test(tile.href) && !tile.over, `a result on the landing has its words and goes to itself: ${JSON.stringify(tile)}`);
  check((await ev(`!document.querySelector(".studio-keep") && /Kept 30 minutes/.test(document.querySelector(".studio-recent-head").textContent)`)), "how long results stay is said once, beside them");
  // A file that is gone shows the tool's icon and "Expired", never a broken icon.
  await ev(`new Promise((done) => { const img = document.querySelector(".studio-recent img"); img.addEventListener("error", () => setTimeout(done, 50), { once: true }); img.src = "/app/lab/results/gone"; })`);
  const gone = await ev(`(() => { const box = document.querySelector(".studio-recent .result-img"); return { img: box.querySelector("img").hidden, shown: !box.querySelector("[data-result-gone]").hidden, text: box.textContent.trim() }; })()`);
  check(gone.img && gone.shown && gone.text === "Expired", `an expired result falls back: ${JSON.stringify(gone)}`);
  await nav("/app/studio/speech");
  check(await ev(`document.querySelectorAll("#lab-items > li").length === 0`), "another tool's tray does not");
  await nav("/app/studio/image");
  for (let i = 0; i < 20 && !(await ev(`!!document.querySelector("#lab-items img")`)); i++) await sleep(250);
  // The fake's PNG is a signature, not a picture: check what the page serves.
  const done = await ev(`(async () => { const li = document.querySelector("#lab-items > li"); const img = li?.querySelector("img"); if (!img) return {};
    const r = await fetch(img.getAttribute("src")); const b = new Uint8Array(await r.arrayBuffer());
    return { type: r.headers.get("content-type"), png: b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47, alt: li.querySelector("dialog img").alt, label: li.querySelector(".image-open").getAttribute("aria-label"), polling: li.hasAttribute("hx-get") }; })()`);
  check(done.type === "image/png" && done.png && /paper boat/.test(done.alt) && /paper boat/.test(done.label) && !done.polling, `the finished image is in its tray after leaving: ${JSON.stringify(done)}`);
  // The viewer: full screen, with its actions; a batch moves between pictures.
  await nav("/app/studio/image");
  await ev(`(() => { const f = document.querySelector("form.lab-dock"); f.querySelector("#lab-prompt").value = "a batch of two"; f.querySelector("input[name=count][value='2']").checked = true; f.querySelector("button[type=submit]").click(); })()`);
  for (let i = 0; i < 24 && (await ev(`document.querySelectorAll("#lab-items dialog.viewer").length`)) < 3; i++) await sleep(250);
  await ev(`document.querySelector("#lab-items .image-open").click()`);
  await sleep(250);
  const view = await ev(`(() => { const d = document.querySelector("dialog.viewer[open]"); const r = d.getBoundingClientRect(); const img = d.querySelector(".viewer-img").getBoundingClientRect(); const tile = document.querySelector(".image-tile").getBoundingClientRect();
    return { full: Math.round(r.width) === innerWidth && Math.round(r.height) === innerHeight, img: img.height > 150, count: d.querySelector("[data-view-count]").textContent, acts: [...d.querySelectorAll(".viewer-foot .viewer-act span")].map((s) => s.textContent).filter(Boolean), small: [...d.querySelectorAll("button, a, summary")].filter((e) => e.getClientRects().length && e.getBoundingClientRect().height < 43.5).length, tile: tile.width > 100 }; })()`);
  check(view.full && view.img && view.tile && view.small === 0 && /of/.test(view.count) && ["Save", "Edit", "Vary", "Details"].every((a) => view.acts.includes(a)), `the viewer fills the screen with its actions: ${JSON.stringify(view)}`);
  const first = await ev(`document.querySelector("dialog.viewer[open]").id`);
  await ev(`document.querySelector("dialog.viewer[open] [data-view-step='1']").click()`);
  check((await ev(`document.querySelector("dialog.viewer[open]").id`)) !== first, "Next moves to another picture");
  // Share: the file leaves without its prompt, at the same size. If encoding
  // fails it says so and offers Save; without file sharing, the Details say so.
  const shareStub = `navigator.canShare = () => true; navigator.share = async (d) => { const b = new Uint8Array(await d.files[0].arrayBuffer()); window.__shared = { type: d.files[0].type, prompt: new TextDecoder("latin1").decode(b).includes("parameters"), w: new DataView(b.buffer).getUint32(16) }; };`;
  const addStub = await send("Page.addScriptToEvaluateOnNewDocument", { source: shareStub });
  await nav("/app/studio/image");
  const shareBtn = `document.querySelector("#lab-items dialog.viewer [data-share]")`;
  const original = await ev(`(async () => { const u = ${shareBtn}.dataset.share; const b = new Uint8Array(await (await fetch(u)).arrayBuffer()); return { prompt: new TextDecoder("latin1").decode(b).includes("parameters"), w: new DataView(b.buffer).getUint32(16) }; })()`);
  check(original.prompt, "the saved file carries its prompt");
  await ev(`${shareBtn}.closest("dialog").showModal(); ${shareBtn}.click()`);
  for (let i = 0; i < 20 && !(await ev(`!!window.__shared`)); i++) await sleep(100);
  const shared = await ev(`({ s: window.__shared, said: document.querySelector("dialog.viewer[open] [data-share-status]").textContent })`);
  check(shared.s?.type === "image/png" && shared.s.prompt === false && shared.s.w === original.w && /without the prompt/.test(shared.said), `Share leaves the prompt out: ${JSON.stringify(shared)}`);
  await ev(`createImageBitmap = async () => { throw new Error("no"); }; window.__shared = null; document.querySelector("dialog.viewer[open]").close(); ${shareBtn}.closest("dialog").showModal(); ${shareBtn}.click()`);
  await sleep(400);
  const failed = await ev(`({ shared: window.__shared, hidden: ${shareBtn}.hidden, said: document.querySelector("dialog.viewer[open] [data-share-status]").textContent })`);
  check(!failed.shared && failed.hidden && /Use Save/.test(failed.said), `a failed encode falls back to Save: ${JSON.stringify(failed)}`);
  await send("Page.removeScriptToEvaluateOnNewDocument", { identifier: addStub.identifier });
  await send("Page.addScriptToEvaluateOnNewDocument", { source: `navigator.canShare = () => false;` }).then((r) => (globalThis.__noShare = r.identifier));
  await nav("/app/studio/image");
  const noShare = await ev(`({ hidden: ${shareBtn}.hidden, note: !document.querySelector("#lab-items dialog.viewer [data-no-share]").hidden })`);
  check(noShare.hidden && noShare.note, `without file sharing the Details say so: ${JSON.stringify(noShare)}`);
  await send("Page.removeScriptToEvaluateOnNewDocument", { identifier: globalThis.__noShare });
  await nav("/app/studio/image");
  await ev(`(() => { document.querySelector("#lab-prompt").value = "fake:slow-image stop me"; document.querySelector(".lab-dock button[type=submit]").click(); })()`);
  for (let i = 0; i < 30 && !(await ev(`[...document.querySelectorAll("#lab-items > li button")].some((b) => b.textContent.trim() === "Stop")`)); i++) await sleep(100);
  await ev(`[...document.querySelectorAll("#lab-items > li button")].find((b) => b.textContent.trim() === "Stop")?.click()`);
  await sleep(1200);
  check(await ev(`document.querySelector("#lab-items > li").textContent.includes("Stopped.")`), "Stop stops the run");
  const before = await ev(`document.querySelectorAll("#lab-items > li").length`);
  await ev(`document.querySelector("#lab-items > li [aria-label=\\"Remove this result\\"]").click()`);
  await sleep(800);
  const after = await ev(`document.querySelectorAll("#lab-items > li").length`);
  const left = await runsNow();
  check(after === before - 1 && left.length === after, `Remove drops the item and its run (${before} → ${after}, runs ${left})`);
  await ev(`document.querySelector("#lab-prompt").focus()`);
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844 - 336, deviceScaleFactor: 2, mobile: true });
  await sleep(500);
  const kb = await ev(`(() => { const d = document.querySelector(".lab-dock").getBoundingClientRect(); return { bottom: Math.round(d.bottom), h: innerHeight, tabbar: getComputedStyle(document.querySelector("nav.tabbar")).display }; })()`);
  check(kb.bottom === kb.h && kb.tabbar === "none", `while typing the prompt sits on the keyboard: ${JSON.stringify(kb)}`);
}
// 8. Unload (§23): with a request in flight the page says it is waiting,
// then the model is unloaded once the request finishes, and the outcome is
// shown and announced.
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
// Start the request from this page (navigating would cancel it).
await nav("/app/models/running");
await ev(`void fetch("/app/api/v1/chat/completions", { method: "POST", headers: { "content-type": "application/json", "x-localbase-ui": "1" }, body: JSON.stringify({ model: "qwen3.5-4b-q4_k_m", stream: true, messages: [{ role: "user", content: "fake:slow 400" }] }) })`);
// Running refreshes every 10 s; its Refresh button shows the state now.
for (let i = 0; i < 30 && !(await ev(`!!document.querySelector("#running-live .pill-warn")`)); i++) {
  await ev(`[...document.querySelectorAll("main button")].find((b) => b.textContent.trim() === "Refresh").click()`);
  await sleep(250);
}
// At the narrowest width a model's name stays whole: the controls move, not the letters.
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 568, deviceScaleFactor: 2, mobile: true });
await sleep(150);
const nameAt320 = await ev(`(() => { const n = document.querySelector("#running-live .running-row a .font-semibold"); const r = document.createRange(); r.selectNodeContents(n); const lines = new Set([...r.getClientRects()].map((c) => Math.round(c.top))).size; return { lines, width: Math.round(n.getBoundingClientRect().width), over: document.documentElement.scrollWidth > document.documentElement.clientWidth }; })()`);
check(nameAt320.lines === 1 && !nameAt320.over, `Running at 320: the model's name stays on one line: ${JSON.stringify(nameAt320)}`);
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await sleep(150);
await ev(`document.querySelector("#running-live [data-open-sheet='unload-sheet']").click()`);
await sleep(300);
const asked = await ev(`({ open: document.getElementById("unload-sheet")?.open, text: document.getElementById("unload-sheet")?.textContent.replace(/\\s+/g, " ") })`);
check(asked.open && /This frees .* Anything running on it finishes first/.test(asked.text), `Unload asks first: ${JSON.stringify(asked)}`);
await sleep(10500); // a poll would have replaced the sheet; it waits while it is open
check(await ev(`document.getElementById("unload-sheet")?.open === true`), "the sheet survives polling");
await ev(`document.querySelector("#unload-sheet button.btn-primary").click()`);
await sleep(400);
const waitingText = await ev(`(() => { const b = document.getElementById("unload-busy"); return b && getComputedStyle(b).display !== "none" ? b.textContent.trim() : ""; })()`);
check(/^Freeing memory…/.test(waitingText), `while draining it says so: ${JSON.stringify(waitingText)}`);
for (let i = 0; i < 40 && !(await ev(`document.getElementById("running-live").textContent.includes("Freed")`)); i++) await sleep(250);
// Announced once the swap settles, a moment after the words are on screen.
for (let i = 0; i < 20 && !(await ev(`document.getElementById("running-status").textContent`)); i++) await sleep(100);
const unloaded = await ev(`({ shown: document.getElementById("running-live").textContent.includes("Freed"), said: document.getElementById("running-status").textContent, empty: !document.getElementById("unload-sheet") })`);
check(unloaded.shown && /^Freed/.test(unloaded.said) && unloaded.empty, `unloaded, shown and announced: ${JSON.stringify(unloaded)}`);
// 9. My voices: add one through the sheet (it closes and the list says so),
// then Play shows as pressed. Needs any 3 to 30 second audio file in
// LBGO_VOICE_FILE.
const voiceFile = process.env.LBGO_VOICE_FILE;
if (!voiceFile) console.log("skip voices (set LBGO_VOICE_FILE to a 3 to 30 second audio file)");
else {
  const voiceName = `Check ${Date.now() % 100000}`;
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await nav("/app/voices");
  await ev(`document.querySelector("[data-open-sheet=voice-add]").click()`);
  await sleep(300);
  const doc = await send("DOM.getDocument", {});
  const input = await send("DOM.querySelector", { nodeId: doc.root.nodeId, selector: "#voice-add input[type=file]" });
  await send("DOM.setFileInputFiles", { nodeId: input.nodeId, files: [voiceFile] });
  for (let i = 0; i < 40 && !(await ev(`!document.querySelector("#voice-add audio[data-audio-preview]").classList.contains("hidden")`)); i++) await sleep(250);
  await ev(`(() => { const f = document.querySelector("#voice-add form"); f.name.value = ${JSON.stringify(voiceName)}; f.consent.checked = true; f.querySelector("button[type=submit]").click(); })()`);
  for (let i = 0; i < 40 && !(await ev(`document.getElementById("voice-list").textContent.includes(${JSON.stringify("Added " + voiceName + ".")})`)); i++) await sleep(250);
  await sleep(300);
  const added = await ev(`({ open: document.getElementById("voice-add").open, listed: [...document.querySelectorAll("#voice-list li")].some((li) => li.textContent.includes(${JSON.stringify(voiceName)})) })`);
  check(!added.open && added.listed, `a voice added through the sheet closes it and joins the list: ${JSON.stringify(added)}`);
  const box = await ev(`(() => { const r = document.querySelector("#voice-list [data-play]").getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
  for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: box.x, y: box.y, button: "left", clickCount: 1 });
  await sleep(600);
  check((await ev(`document.querySelector("#voice-list [data-play]").getAttribute("aria-pressed")`)) === "true", "Play shows as pressed while it plays");
  // Leave nothing behind: a root holds a limited number of voices.
  await ev(`(async () => { const list = await (await fetch("/app/api/_localbase/voices", { headers: { "x-localbase-ui": "1" } })).json(); const v = (list.data ?? []).find((x) => x.name === ${JSON.stringify(voiceName)}); if (v) await fetch("/app/api/_localbase/voices/" + v.id, { method: "DELETE", headers: { "x-localbase-ui": "1" } }); })()`);
}
// --- Decisions (DESIGN §22.17) --------------------------------------------------
await nav("/app/studio/decisions");
if (!(await ev(`!!document.getElementById("decide")`))) {
  console.log("skip decisions (no decision model selected)");
} else {
  const typeTop = `document.querySelector(".decide-types").getBoundingClientRect().top + scrollY`; // in the page: focus can scroll it
  const pick = (id) => `(async () => { document.querySelector("[data-open-sheet=decide-template]").click(); await new Promise((r) => setTimeout(r, 50)); document.querySelector('#decide-template [data-template="${id}"]').click(); })()`;
  const situation = (text) => `(() => { const a = document.getElementById("decide-situation"); a.value = ${JSON.stringify(text)}; a.dispatchEvent(new Event("input", { bubbles: true })); })()`;
  const decide = `document.querySelector("#decide button[type=submit]").click()`;
  const finished = async () => { for (let i = 0; i < 60 && (await ev(`!document.querySelector("[data-result] .decide-verdict") && !document.querySelector("[data-result] [role=alert]")`)); i++) await sleep(100); };
  const read = `({ word: document.querySelector("[data-result] .decide-word")?.textContent, verdict: document.querySelector("[data-result] .decide-verdict")?.textContent, labels: [...document.querySelectorAll("[data-result] .decide-bar-name")].map((n) => n.textContent), said: document.getElementById("decide-said").textContent })`;
  const decisionRuns = async () => (await ev(`fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => (b.data ?? b).filter((x) => x.label === "Decision").map((x) => x.status))`));

  await ev(pick("route"));
  check((await decisionRuns()).length === 0 && (await ev(`document.getElementById("lab-prompt").value`)).includes("team") && !(await ev(`document.querySelector("[data-result]").hidden === false`)), "a template fills the form and runs nothing");

  const top = await ev(typeTop);
  await ev(`document.querySelector("input[name=decide-type][value=noul]").click()`);
  check((await ev(typeTop)) === top && (await ev(`document.querySelector("[data-options]").hidden`)), "the question type stays put; yes or no needs no options");
  await ev(`document.querySelector("input[name=decide-type][value=choice]").click()`);

  await ev(decide);
  await finished();
  check((await ev(`location.search`)) === "", "the question stays out of the address");
  const strong = await ev(read);
  check(strong.word === "Strongly favored" && /^Billing/.test(strong.verdict) && strong.labels[0] === "Billing" && /Strongly favored: Billing, 91%/.test(strong.said), `words first, then ranked odds, and announced: ${JSON.stringify(strong)}`);
  check((await ev(`document.querySelector("[data-result]").textContent.includes("not a check that it is right")`)), "the standing line is under the answer");
  const led = await ev(`(() => { const q = document.querySelector("[data-summary-question]"), word = document.querySelector("[data-result] .decide-word"), bar = document.querySelector("[data-bar-answer]"); return { form: document.querySelector("[data-form-open]").hidden, summary: !document.querySelector("[data-form-summary]").hidden, tabs: document.querySelector(".decide-types").getBoundingClientRect().height, question: q.textContent, first: q.getBoundingClientRect().bottom <= word.getBoundingClientRect().top, ask: document.querySelector("[data-bar-form]").hidden, again: !bar.hidden && !bar.querySelector("[data-again]").hidden }; })()`);
  check(led.form && led.summary && led.tabs === 0 && /team/.test(led.question) && led.first && led.ask && led.again, `the answer is led by its question, without the form or its tabs: ${JSON.stringify(led)}`);
  check((await ev(`document.querySelector(".decide-keep").textContent.includes("stay in this browser")`)), "it says questions stay in this browser, so nobody looks for them in Recent");
  await sleep(200);
  check((await decisionRuns()).length === 0, "the run is deleted once its answer is here");

  await ev(`document.querySelector("[data-code]:not([data-form-open] *)").click()`);
  const code = await ev(`({ open: document.getElementById("decide-code").open, text: document.querySelector("[data-code-text]").textContent })`);
  check(code.open && code.text.includes("$LOCALBASE_API_KEY") && code.text.includes("/v1/systemone") && code.text.includes("Billing"), "View code shows the request with the key as a placeholder");
  await ev(`document.querySelector("input[name=decide-code-lang][value=js]").click()`);
  check((await ev(`document.querySelector("[data-code-text]").textContent.includes("process.env.LOCALBASE_API_KEY")`)), "and JavaScript");
  await ev(`document.getElementById("decide-code").close()`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:close it could be either"));
  await ev(decide);
  await sleep(300); await finished();
  const close = await ev(read);
  check(close.word === "Close call" && /^Billing or Shipping/.test(close.verdict), `a close call names both: ${close.verdict}`);
  const tracks = await ev(`(() => { const rows = [...document.querySelectorAll("[data-result] .decide-bar")], left = (r, q) => r.querySelector(q).getBoundingClientRect(); return { widths: rows.map((r) => Math.round(left(r, ".decide-track").width)), order: rows.every((r) => left(r, ".decide-bar-name").right <= left(r, ".decide-track").left && left(r, ".decide-track").right <= left(r, ".decide-bar-odds").left), numbers: new Set(rows.map((r) => Math.round(left(r, ".decide-bar-odds").right))).size, tabular: getComputedStyle(rows[0].querySelector(".decide-bar-odds")).fontVariantNumeric, note: document.querySelector("[data-result] .decide-note")?.textContent, leads: document.querySelectorAll("[data-result] .decide-bar-top").length }; })()`);
  check(new Set(tracks.widths).size === 1 && tracks.widths[0] > 0 && tracks.order && tracks.numbers === 1 && tracks.tabular.includes("tabular-nums") && /a person’s look/.test(tracks.note) && tracks.leads === 2, `a close result: equal tracks, then numbers after them, lined up and tabular; both lead, and it says to look: ${JSON.stringify(tracks)}`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:unsure"));
  await ev(decide);
  await sleep(300); await finished();
  check((await ev(read)).word === "Unsure", "no answer past half reads Unsure");

  await ev(pick("urgent"));
  await ev(decide);
  await sleep(300); await finished();
  const scale = await ev(read);
  check(scale.labels.join() === "Low,Medium,High" && /^Low/.test(scale.verdict), `a scale keeps its order: ${scale.labels}`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(`(() => { document.querySelector("input[name=decide-type][value=choice]").click(); for (const n of document.querySelectorAll("[data-remove-option]")) document.querySelector("[data-remove-option]")?.click(); })()`);
  await ev(decide);
  const none = await ev(`({ error: document.getElementById("decide-error").textContent, q: document.getElementById("lab-prompt").value })`);
  check(/at least two options/.test(none.error) && none.q.length > 0, `too few options: says so and keeps the question: ${none.error}`);

  // Stop, and finding a run again after a reload.
  await ev(pick("route"));
  await ev(situation("fake:slow one"));
  await ev(decide);
  await sleep(300);
  check((await ev(`!!document.querySelector("[data-result] [aria-busy] .skeleton") && !!document.querySelector("[data-stop]")`)), "while deciding: its place, and Stop");
  await ev(`document.querySelector("[data-stop]").click()`);
  await sleep(400);
  check((await ev(`document.querySelector("[data-result]").hidden && !document.querySelector("[data-form-open]").hidden`)) && (await ev(`document.getElementById("decide-situation").value`)).includes("fake:slow"), "Stop returns to the form with its text");
  await ev(decide);
  await sleep(400);
  await nav("/app/studio/decisions");
  check((await ev(`!!document.querySelector("[data-result] [aria-busy]")`)) || (await ev(`!!document.querySelector("[data-result] .decide-verdict")`)), "a reload finds the question still running");
  await sleep(1000);
  for (let i = 0; i < 60 && !(await ev(`!!document.querySelector("[data-result] .decide-verdict")`)); i++) await sleep(100);
  check((await ev(read)).word === "Strongly favored", "and then its answer");

  // Saved examples stay in this browser.
  await ev(`document.querySelector("[data-save]").click()`);
  await nav("/app/studio/decisions");
  await ev(`document.querySelector("[data-open-sheet=decide-template]").click()`);
  check((await ev(`document.querySelector("#decide-template [data-saved] [data-template]")?.textContent.includes("Which team")`)), "a saved example is there after a reload");
  await ev(`document.querySelector("#decide-template [data-unsave]").click()`);
  check((await ev(`document.querySelector("[data-saved-box]").hidden`)), "and can be deleted");
  await ev(`document.getElementById("decide-template").close()`);

  // After an answer: the same type and options, with a new question.
  await ev(`document.querySelector("[data-again]").click()`);
  const again = await ev(`({ q: document.getElementById("lab-prompt").value, s: document.getElementById("decide-situation").value, focus: document.activeElement?.id, open: !document.querySelector("[data-form-open]").hidden, tabs: document.querySelector(".decide-types").getBoundingClientRect().height > 0, options: document.querySelectorAll("[data-option][data-part=name]").length, result: document.querySelector("[data-result]").hidden })`);
  check(again.q === "" && again.s === "" && again.focus === "lab-prompt" && again.open && again.tabs && again.options >= 2 && again.result, `Ask another keeps the options and starts at the question: ${JSON.stringify(again)}`);

  // A batch.
  await ev(pick("route"));
  await ev(`document.querySelector("[data-batch-chip]").click()`);
  await ev(situation(Array.from({ length: 201 }, (_, i) => `line ${i}`).join("\n")));
  await ev(decide);
  await sleep(200);
  check(/at most 200/.test(await ev(`document.getElementById("decide-error").textContent`)) && (await ev(`document.querySelector("[data-batch]").hidden`)), "201 lines are refused, and say why");
  await ev(situation("one\nfake:close two\nfake:unsure three\n=SUM(1)\nfive"));
  check((await ev(`document.querySelector("[data-situation-count]").textContent`)) === "5 situations", "a batch counts its lines");
  await ev(decide);
  for (let i = 0; i < 80 && (await ev(`!/5 of 5/.test(document.querySelector("[data-batch-summary]")?.textContent ?? "")`)); i++) await sleep(100);
  const summary = await ev(`document.querySelector("[data-batch-summary]").textContent`);
  check(summary === "5 of 5 done, 2 to review", `the batch says how it went: ${summary}`);
  await ev(`document.querySelector("input[name=decide-filter][value=review]").click()`);
  const visible = await ev(`[...document.querySelectorAll(".decide-row:not(.decide-row-head)")].filter((n) => !n.hidden).map((n) => n.querySelector(".decide-word").textContent)`);
  check(visible.join() === "Close call,Unsure", `the filter leaves close calls and unsure: ${visible}`);
  await ev(`document.querySelector("input[name=decide-filter][value=all]").click()`);
  check((await ev(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)), "the batch fits the phone");
  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:slow a\nfake:slow b\nfake:slow c"));
  await ev(decide);
  // Until the first row is really done, not for as long as a row usually takes.
  for (let i = 0; i < 200 && !(await ev(`/^1 of 3/.test(document.querySelector("[data-batch-summary]")?.textContent ?? "")`)); i++) await sleep(50);
  await ev(`document.querySelector("[data-batch-stop]").click()`);
  await sleep(500);
  const stopped = await ev(`({ s: document.querySelector("[data-batch-summary]").textContent, more: !document.querySelector("[data-batch-continue]").hidden })`);
  check(/^1 of 3 done/.test(stopped.s) && stopped.more, `Stop keeps finished rows and offers Continue: ${JSON.stringify(stopped)}`);
  // A lost connection pauses a batch rather than failing every row left. The batch has to be running when the connection
  // goes, with rows left, or the rest says nothing: an empty line reads the same on a batch that is still working.
  const batchNow = `(() => { const q = (s) => document.querySelector(s); return { running: !q("[data-batch-stop]").hidden, s: q("[data-batch-summary]").textContent, paused: q("[data-batch-paused]").textContent, more: !q("[data-batch-continue]").hidden, failed: /failed/.test(q("[data-batch-summary]").textContent) }; })()`;
  await ev(`document.querySelector("[data-batch-continue]").click()`);
  for (let i = 0; i < 40 && !(await ev(`${batchNow}.running`)); i++) await sleep(25);
  const going = await ev(batchNow);
  check(going.running && !going.paused && !going.more && /^1 of 3/.test(going.s), `the batch is still running, with rows left, when the connection goes: ${JSON.stringify(going)}`);
  await send("Network.enable");
  await send("Network.emulateNetworkConditions", { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
  const droppedAt = Date.now();
  // If the row's stream is already open the pause comes when the row ends. If it isn't, the page tries it for 30 s first.
  for (let i = 0; i < 450 && !(await ev(`/Lost the connection/.test(document.querySelector("[data-batch-paused]").textContent)`)); i++) await sleep(100);
  const waited = Date.now() - droppedAt;
  const dropped = await ev(batchNow);
  await send("Network.emulateNetworkConditions", { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
  check(/Lost the connection/.test(dropped.paused) && !dropped.running && dropped.more && !dropped.failed && /^[12] of 3/.test(dropped.s), `a lost connection pauses the running batch, with rows left: ${JSON.stringify({ ...dropped, waited })}`);
}

// --- On Decisions, what takes focus clears the bar, and a row's parts stay in a line (DESIGN §27.4) --
// Real Tab presses, as Reliability's suite found this: focus on a control left only its top edge above
// the dock. At each width, in the form and on an answer.
await nav("/app/studio/decisions");
if (!(await ev(`!!document.getElementById("decide")`))) console.log("skip decision answers (no decision model selected)");
if (await ev(`!!document.getElementById("decide")`)) {
  const pressTab = async () => {
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Tab", code: "Tab", windowsVirtualKeyCode: 9, nativeVirtualKeyCode: 9 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Tab", code: "Tab", windowsVirtualKeyCode: 9, nativeVirtualKeyCode: 9 });
    await sleep(70);
  };
  const focused = `(() => { const a = document.activeElement, dock = document.querySelector("[data-docked]"); if (!a || a === document.body) return null; const inMain = !!a.closest("#decide") && !a.closest("[data-docked]") && !a.closest("dialog"); if (!inMain) return { skip: true }; const r = a.getBoundingClientRect(), d = dock && !dock.hidden ? dock.getBoundingClientRect().top : innerHeight; return { what: (a.getAttribute("aria-label") || a.textContent || a.placeholder || a.tagName).trim().slice(0, 24), top: Math.round(r.top), bottom: Math.round(r.bottom), dockTop: Math.round(d), clear: r.bottom <= d + 0.5 && r.top >= 0 }; })()`;
  const tabThrough = async (label) => {
    await ev(`(document.activeElement?.blur(), scrollTo(0, 0))`);
    const covered = []; let seen = 0;
    for (let i = 0; i < 45; i++) {
      await pressTab();
      const f = await ev(focused);
      if (!f || f.skip) continue;
      seen++;
      if (!f.clear) covered.push(f);
    }
    check(seen >= 3 && covered.length === 0, `${label}: each of ${seen} controls clears the bar when it takes focus${covered.length ? ": " + JSON.stringify(covered).slice(0, 260) : ""}`);
  };
  const pickRoute = `(async () => { document.querySelector("[data-open-sheet=decide-template]").click(); await new Promise((r) => setTimeout(r, 60)); document.querySelector('#decide-template [data-template="route"]').click(); })()`;
  for (const [width, height] of [[360, 740], [390, 844], [430, 932]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    await nav("/app/studio/decisions");
    await ev(pickRoute);
    await sleep(150);
    await tabThrough(`at ${width}, the form`);
    await ev(`document.querySelector("#decide .lab-dock button[type=submit]").click()`);
    for (let i = 0; i < 60 && !(await ev(`!!document.querySelector("[data-result] .decide-word:not(:empty) ~ .decide-verdict")`)); i++) await sleep(100);
    await tabThrough(`at ${width}, an answer`);
    const row = await ev(`(() => { const r = document.querySelector("[data-result] .decide-bar"), name = r.querySelector(".decide-bar-name").getBoundingClientRect(), track = r.querySelector(".decide-track").getBoundingClientRect(), odds = r.querySelector(".decide-bar-odds").getBoundingClientRect(); return { oneLine: Math.abs((name.top + name.bottom) / 2 - (track.top + track.bottom) / 2) < 8 && Math.abs((odds.top + odds.bottom) / 2 - (track.top + track.bottom) / 2) < 8, over: document.documentElement.scrollWidth > document.documentElement.clientWidth }; })()`);
    check(row.oneLine && !row.over, `at ${width}, a row's name, track and number share a line, on any browser: ${JSON.stringify(row)}`);
  }
}

// --- Video (DESIGN §22.18) ------------------------------------------------------
await nav("/app/studio/video");
if (!(await ev(`!!document.querySelector("form[aria-label='Make a video']")`))) {
  console.log("skip video (no video model selected)");
} else {
  const makeVideo = (text) => `(() => { const p = document.getElementById("lab-prompt"); p.value = ${JSON.stringify(text)}; document.querySelector("form[aria-label='Make a video'] button[type=submit]").click(); })()`;
  const itemFor = (text) => `[...document.querySelectorAll("#lab-items .lab-item-video")].find((n) => n.textContent.includes(${JSON.stringify(text)}))`;
  const waitItem = async (text, test, ms = 30000) => { const t = Date.now(); let got; while (Date.now() - t < ms) { got = await ev(`(() => { const n = ${itemFor(text)}; return n ? { text: n.textContent.replace(/\\s+/g, " "), active: n.hasAttribute("data-poll"), video: !!n.querySelector("video"), alert: n.querySelector("[role=alert]")?.textContent ?? "" } : null; })()`); if (got && test(got)) return got; await sleep(200); } return got; };

  const sheet = await ev(`(() => { document.querySelector("[data-video-chip]").click(); const t = document.getElementById("lab-more").textContent.replace(/\\s+/g, " "); document.getElementById("lab-more").close(); return { t, chip: document.querySelector("[data-video-chip]").textContent.replace(/\\s+/g, " ").trim(), model: !!document.querySelector("[data-open-sheet=lab-model]") }; })()`);
  check(/About \d+ seconds, tall/.test(sheet.t) && /up to 10 minutes/.test(sheet.t) && !/fps|frames/.test(sheet.t) && !sheet.model && sheet.chip === "Start from Words", `the profile is in words, and one model needs no chooser: ${sheet.chip}`);

  await ev(makeVideo("fake:slow a paper boat"));
  const running = await waitItem("fake:slow a paper boat", (g) => g.active);
  check(running?.active && /Waiting for its turn|Making the video/.test(running.text) && /it keeps going/.test(running.text) && !/\d%/.test(running.text), `a job shows its stage, no percentage: ${running?.text.slice(0, 90)}`);
  const place = await ev(`(() => { const r = document.querySelector("#lab-items .lab-item-video .video-frame").getBoundingClientRect(); return r.height / r.width; })()`);
  check(place > 1.5 && place < 2, `its place has the final shape (${place.toFixed(2)})`);
  check((await ev(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)), "it fits the phone");
  const stop = await ev(`(() => { const b = ${itemFor("fake:slow a paper boat")}.querySelector("[hx-post$='/cancel']"); window.scrollTo(0, 0); const dock = document.querySelector(".lab-dock").getBoundingClientRect(), text = document.querySelector("#lab-items .lab-progress span").getBoundingClientRect(), frame = document.querySelector("#lab-items .lab-progress").getBoundingClientRect(); return { firstScreen: b.getBoundingClientRect().bottom <= dock.top, centered: Math.abs((text.top + text.bottom) / 2 - (frame.top + frame.bottom) / 2) < 8 }; })()`);
  check(stop.firstScreen && stop.centered, `Stop is on the first screen above the prompt, and the stage is centered: ${JSON.stringify(stop)}`);
  await ev(makeVideo("a second one"));
  await sleep(700);
  const busy = await ev(`({ said: document.querySelector("#lab-result [role=alert]")?.textContent, kept: document.getElementById("lab-prompt").value })`);
  check(/One video at a time/.test(busy.said ?? "") && busy.kept === "a second one", `a second video says what to do and keeps its words: ${JSON.stringify(busy)}`);

  await nav("/app/models");
  await sleep(600);
  const pill = await ev(`(() => { const p = [...document.querySelectorAll("[data-run-pill]")].find((n) => !n.hidden); return p ? { href: p.getAttribute("href"), text: p.textContent.trim() } : null; })()`);
  check(pill?.href === "/app/studio/video" && /Video/.test(pill.text), `the pill leads back: ${JSON.stringify(pill)}`);
  await nav("/app/studio");
  check((await ev(`!!document.querySelector('.studio-recent a[href^="/app/studio/video"]')`)), "the landing's Recent shows it");
  await nav("/app/studio/video");
  check((await waitItem("fake:slow a paper boat", (g) => g.active))?.active, "it is still there after leaving");
  check((await ev(`[...document.querySelectorAll("[data-run-pill]")].every((n) => n.hidden)`)), "no pill on its own page");

  await ev(`${itemFor("fake:slow a paper boat")}.querySelector("[hx-post$='/cancel']").click()`);
  // With no job running, a polled page asks about videos rarely.
  await waitItem("fake:slow a paper boat", (g) => !g.active);
  await nav("/app/models/running");
  await sleep(7000);
  const asks = await ev(`performance.getEntriesByName(location.origin + "/app/lab/active").length`);
  check(asks <= 1, `a polled page does not poll for videos: ${asks} request(s) in 8 s`);
  await nav("/app/studio/video");
  const stopped = await waitItem("fake:slow a paper boat", (g) => !g.active);
  check(/Stopped\. Nothing was saved\./.test(stopped?.text ?? "") && /Try again/.test(stopped.text), `Stop says what happened: ${stopped?.text.slice(0, 90)}`);

  await ev(makeVideo("a quiet beach"));
  const done = await waitItem("a quiet beach", (g) => g.video);
  check(done?.video, "the finished video is in the tray");
  await sleep(600);
  const played = await ev(`(async () => { const v = ${itemFor("a quiet beach")}.querySelector("video"); if (v.readyState < 1) await new Promise((r) => { v.onloadedmetadata = r; v.onerror = r; setTimeout(r, 4000); }); return { duration: v.duration, width: v.videoWidth, save: !!${itemFor("a quiet beach")}.querySelector("a[download='video.mp4']"), again: /Make again/.test(${itemFor("a quiet beach")}.textContent) }; })()`);
  check(played.duration > 1 && played.width > 0 && played.save && played.again, `it plays, with Save and Make again: ${JSON.stringify(played)}`);
  // A finished video's tile on the landing says what it is, and shows its first frame.
  await nav("/app/studio");
  const tile = await ev(`(async () => { const a = document.querySelector('.studio-recent a[href^="/app/studio/video"]'), v = a?.querySelector("video"); if (v && v.readyState < 2) await new Promise((r) => { v.onloadeddata = r; v.onerror = r; setTimeout(r, 4000); }); return { badge: a?.querySelector(".studio-tile-badge")?.textContent.trim(), frame: v?.readyState >= 2 && v?.videoWidth > 0, hidden: a?.querySelector(".studio-tile-badge")?.getAttribute("aria-hidden"), named: a?.querySelector(".studio-tile-meta")?.textContent.trim() }; })()`);
  check(/^\d+ s$/.test(tile.badge ?? "") && tile.frame && tile.hidden === "true" && /^Video, \d+ s · /.test(tile.named ?? ""), `a finished video's tile shows a frame, its type and its length: ${JSON.stringify(tile)}`);
  await nav("/app/studio/video");

  await ev(makeVideo("fake:fail a bad one"));
  const failed = await waitItem("fake:fail a bad one", (g) => !g.active);
  check(failed?.alert && /Try again/.test(failed.text), `a failure says so in words and offers Try again: ${failed?.alert}`);
  const before = await ev(`document.querySelectorAll("#lab-items .lab-item-video").length`);
  // htmx wires a swapped-in button when it settles; a click sooner does nothing, and no person is that quick.
  const retry = `[...${itemFor("fake:fail a bad one")}.querySelectorAll("button")].find((b) => b.textContent === "Try again")`;
  for (let i = 0; i < 50 && !(await ev(`!!${retry}["htmx-internal-data"]`)); i++) await sleep(50);
  await ev(`${retry}.click()`);
  for (let i = 0; i < 30 && (await ev(`document.querySelectorAll("#lab-items .lab-item-video").length`)) === before; i++) await sleep(200);
  const again = await ev(`({ count: document.querySelectorAll("#lab-items .lab-item-video").length, said: document.querySelector("#lab-result [role=alert]")?.textContent ?? "" })`);
  check(again.count === before + 1, `Try again starts another job: ${JSON.stringify(again)}`);

  const usable = await ev(`document.querySelectorAll("[data-video-use]").length`);
  if (usable) {
    await ev(`document.querySelector("[data-video-chip]").click(); document.querySelector("[data-video-use]").click()`);
    await sleep(600);
    const pic = await ev(`({ chip: document.querySelector("[data-video-chip-value]").textContent, files: document.querySelector("[data-video-source]").files.length, w: document.querySelector("[data-video-preview]").naturalWidth, h: document.querySelector("[data-video-preview]").naturalHeight, clear: !document.querySelector("[data-video-clear]").hidden })`);
    check(pic.chip === "Picture" && pic.files === 1 && pic.w === 480 && pic.h === 832 && pic.clear, `a recent picture becomes the start, cropped to the frame: ${JSON.stringify(pic)}`);
    await ev(`document.querySelector("[data-video-clear]").click()`);
    await sleep(100);
    check((await ev(`document.querySelector("[data-video-chip-value]").textContent === "Words" && document.querySelector("[data-video-source]").files.length === 0`)), "and can be removed");
    await ev(`document.getElementById("lab-more").close()`);
  } else console.log("skip recent picture (no image result to use)");

  await ev(`${itemFor("a quiet beach")}.querySelector("[hx-post$='/remove']").click()`);
  await sleep(500);
  check(!(await ev(`!!${itemFor("a quiet beach")}`)), "Remove takes it out of the tray");
}

// --- Speech (DESIGN §22.19) -----------------------------------------------------
await nav("/app/studio/speech");
if (!(await ev(`!!document.querySelector("form[aria-label='Make speech']")`))) {
  console.log("skip speech (no speech model selected)");
} else {
  const typeScript = (text) => `(() => { const p = document.getElementById("lab-prompt"); p.value = ${JSON.stringify(text)}; p.dispatchEvent(new Event("input", { bubbles: true })); })()`;
  const sendScript = `document.querySelector("form[aria-label='Make speech'] button[type=submit]").click()`;
  const firstItem = `document.querySelector("#lab-items .lab-item-speech")`;
  const itemState = async () => ev(`(() => { const n = ${firstItem}; return n ? { text: n.textContent.replace(/\\s+/g, " ").trim(), active: n.hasAttribute("data-poll"), parts: n.querySelector("progress")?.max ?? 0, audio: !!n.querySelector("audio"), bars: n.querySelectorAll("[data-wave] .speech-wave-base rect").length } : null; })()`);
  const until = async (test, ms = 40000) => { const t = Date.now(); let s; while (Date.now() - t < ms) { s = await itemState(); if (s && test(s)) return s; await sleep(150); } return s; };
  const sentence = "fake:slow The harbor was quiet this morning, and the boats were still. ";

  // The count, and text is never cut in the field.
  await ev(typeScript(sentence.repeat(20)));
  check(/^About \d+ parts$/.test(await ev(`document.querySelector("[data-script-count]").textContent`)), "a long script says it will be made in parts");
  await ev(typeScript("x".repeat(5001)));
  const over = await ev(`({ count: document.querySelector("[data-script-count]").textContent, kept: document.getElementById("lab-prompt").value.length })`);
  check(over.count === "5,001 / 5,000" && over.kept === 5001, `the count from near the limit, nothing cut in the field: ${JSON.stringify(over)}`);
  await ev(sendScript);
  await sleep(500);
  check(/Nothing was cut/.test(await ev(`document.querySelector("#lab-result [role=alert]")?.textContent ?? ""`)) && (await ev(`document.getElementById("lab-prompt").value.length`)) === 5001, "over the limit it says so, and the script stays");

  const digitsNote = async (text) => { await ev(typeScript(text)); return ev(`!document.querySelector("[data-digits-note]").hidden`); };
  check((await digitsNote("1000, 1001, 1002, 1003, 1004")) && !(await digitsNote("Call me on the 12th, about 3 boats and 40 nets, in the morning.")) && !(await digitsNote("")), "the digits note shows only for a script of mostly digits");

  // The voice picker.
  await ev(`document.querySelector("[data-open-sheet=lab-voice]").click()`);
  const picker = await ev(`({ open: document.getElementById("lab-voice").open, radios: document.querySelectorAll("#lab-voices input[type=radio]").length, plays: document.querySelectorAll("#lab-voices [data-voice-preview]").length, add: !!document.querySelector("#lab-voices [data-open-sheet=voice-add]") })`);
  check(picker.open && picker.radios > 1 && picker.plays === picker.radios && picker.add, `each voice has a separate Play, and Add a voice is there: ${JSON.stringify(picker)}`);
  const clickSample = async (i) => { const b = await ev(`(() => { const r = document.querySelectorAll("#lab-voices [data-voice-preview]")[${i}]; r.scrollIntoView({ block: "center" }); const w = r.getBoundingClientRect(); return { x: w.left + w.width / 2, y: w.top + w.height / 2 }; })()`); for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: b.x, y: b.y, button: "left", clickCount: 1 }); };
  await sleep(400); // the sheet has slid in
  const chosenBefore = await ev(`document.querySelector("#lab-voices input[type=radio]:checked")?.value`);
  await clickSample(0);
  let pressed = 0;
  for (let i = 0; i < 40 && !pressed; i++) { await sleep(150); if (/Playing/.test(await ev(`document.querySelector("[data-voice-status]").textContent`))) pressed = 1; }
  check(pressed === 1, "a sample plays");
  await clickSample(1);
  await sleep(200);
  check((await ev(`[...document.querySelectorAll("#lab-voices [data-voice-preview]")].filter((b) => b.getAttribute("aria-pressed") === "true").length`)) === 1, "one sample at a time");
  check((await ev(`document.querySelector("#lab-voices input[type=radio]:checked")?.value`)) === chosenBefore, "playing a sample does not choose the voice");

  if (voiceFile) {
    const b64 = Buffer.from(await Bun.file(voiceFile).arrayBuffer()).toString("base64");
    await ev(`document.querySelector("#lab-voices [data-open-sheet=voice-add]").click()`);
    await ev(`(async () => { const bin = atob(${JSON.stringify(b64)}); const a = new Uint8Array(bin.length); for (let i = 0; i < bin.length; i++) a[i] = bin.charCodeAt(i); const form = document.querySelector("#voice-add form"); form.querySelector("[name=name]").value = "Added Here"; form.querySelector("[name=consent]").checked = true; const t = new DataTransfer(); t.items.add(new File([a], "tone.wav", { type: "audio/wav" })); const input = form.querySelector("[data-audio-input]"); input.files = t.files; input.dispatchEvent(new Event("change", { bubbles: true })); })()`);
    await sleep(1500);
    await ev(`document.querySelector("#voice-add button[type=submit]").click()`);
    let added = null;
    for (let i = 0; i < 40; i++) { await sleep(200); added = await ev(`(() => { const c = document.querySelector("#lab-voices input[type=radio]:checked"); return { checked: c?.dataset.label, beforeBuiltIn: [...document.querySelectorAll("#lab-voices input[type=radio]")].map((r) => r.dataset.label).indexOf("Added Here") < [...document.querySelectorAll("#lab-voices input[type=radio]")].map((r) => r.dataset.label).indexOf("Default"), open: document.getElementById("voice-add").open, chip: document.querySelector("[data-chip-label=voice]").textContent }; })()`); if (added.checked === "Added Here") break; }
    check(added?.checked === "Added Here" && added.beforeBuiltIn && !added.open && added.chip === "Added Here", `a voice added here is chosen, above the model's own: ${JSON.stringify(added)}`);
    await ev(`(async () => { const list = await (await fetch("/app/api/_localbase/voices", { headers: { "x-localbase-ui": "1" } })).json(); const v = (list.data ?? []).find((x) => x.name === "Added Here"); if (v) await fetch("/app/api/_localbase/voices/" + v.id, { method: "DELETE", headers: { "x-localbase-ui": "1" } }); })()`);
    await nav("/app/studio/speech");
  } else {
    await ev(`document.getElementById("lab-voice").close()`);
    console.log("skip adding a voice (set LBGO_VOICE_FILE)");
  }

  // A long script: real progress, it survives leaving, and it comes back joined.
  await ev(typeScript(sentence.repeat(70)));
  await ev(sendScript);
  const started = await until((s) => s.active);
  check(started?.active && started.parts > 5 && /Making part \d+ of \d+/.test(started.text) && /it keeps going/.test(started.text), `it shows real progress: ${started?.text.slice(0, 60)}, ${started?.parts} parts`);
  await nav("/app/models");
  await sleep(500);
  const speechPill = await ev(`(() => { const p = [...document.querySelectorAll("[data-run-pill]")].find((n) => !n.hidden); return p ? { href: p.getAttribute("href"), text: p.textContent.trim() } : null; })()`);
  check(speechPill?.href === "/app/studio/speech", `the pill leads back: ${JSON.stringify(speechPill)}`);
  await send("Page.reload");
  await sleep(800);
  await nav("/app/studio/speech");
  const finished = await until((s) => !s.active);
  check(finished?.audio && finished.bars === 64 && /Make again/.test(finished.text), `it finishes after leaving and reloading, with a waveform: ${finished?.text.slice(0, 80)}`);
  const audio = await ev(`(async () => { const a = ${firstItem}.querySelector("audio"); if (a.readyState < 1) await new Promise((r) => { a.onloadedmetadata = r; a.onerror = r; setTimeout(r, 4000); }); const w = ${firstItem}.querySelector("[data-wave]").getBoundingClientRect(); return { duration: a.duration, x: w.left + w.width / 2, y: w.top + w.height / 2 }; })()`);
  check(audio.duration > 60, `the parts are joined into one file (${audio.duration?.toFixed(0)} s)`);
  await ev(`${firstItem}.querySelector("[data-wave]").scrollIntoView({ block: "center" })`);
  const box = await ev(`(() => { const w = ${firstItem}.querySelector("[data-wave]").getBoundingClientRect(); return { x: w.left + w.width / 2, y: w.top + w.height / 2 }; })()`);
  for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: box.x, y: box.y, button: "left", clickCount: 1 });
  await sleep(300);
  const seek = await ev(`(() => { const a = ${firstItem}.querySelector("audio"); return { at: a.currentTime / a.duration, played: ${firstItem}.querySelector("[data-wave]").style.getPropertyValue("--played") }; })()`);
  check(seek.at > 0.4 && seek.at < 0.6 && parseFloat(seek.played) > 40, `tapping the waveform moves the player: ${JSON.stringify(seek)}`);

  // The cut flag is said.
  await ev(typeScript("fake:cut 1234567890"));
  await ev(sendScript);
  const cut = await until((s) => !s.active && /fake:cut/.test(s.text));
  check(/cut part of this short/.test(cut?.text ?? ""), `speech cut by the model says so: ${cut?.text.slice(0, 70)}`);

  // Stop keeps what was made.
  await ev(typeScript(sentence.repeat(70)));
  await ev(sendScript);
  await until((s) => s.active && /Making part [3-9]/.test(s.text));
  await ev(`${firstItem}.querySelector("[hx-post$='/stop']").click()`);
  const stoppedSpeech = await until((s) => !s.active);
  check(/Stopped after part \d+ of \d+/.test(stoppedSpeech?.text ?? "") && stoppedSpeech.audio, `Stop keeps what was made: ${stoppedSpeech?.text.slice(0, 70)}`);
  await ev(`${firstItem}.querySelector("[hx-post$='/remove']").click()`);
  await sleep(400);
  check(!/Stopped after/.test((await itemState())?.text ?? ""), "Remove takes it out of the tray");
  check((await ev(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)), "it fits the phone");
}

// --- Transcribe (DESIGN §22.20) ---------------------------------------------------
await nav("/app/studio/transcribe");
if (!(await ev(`!!document.querySelector("form[data-transcribe]")`))) {
  console.log("skip transcribe (no transcription model selected)");
} else {
  const trItem = `document.querySelector("#lab-items .lab-item-transcribe")`;
  const trState = () => ev(`(() => { const n = ${trItem}; return n ? { text: n.textContent.replace(/\\s+/g, " ").trim(), active: n.hasAttribute("data-poll"), times: n.querySelectorAll("[data-seek]").length, audio: !!n.querySelector("audio") } : null; })()`);
  const trUntil = async (test, ms = 20000) => { const t = Date.now(); let s; while (Date.now() - t < ms) { s = await trState(); if (s && test(s)) return s; await sleep(150); } return s; };
  const mouse = async (selector) => { const b = await ev(`(() => { const r = document.querySelector(${JSON.stringify(selector)}); r.scrollIntoView({ block: "center" }); const w = r.getBoundingClientRect(); return { x: w.left + w.width / 2, y: w.top + w.height / 2 }; })()`); for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: b.x, y: b.y, button: "left", clickCount: 1 }); };
  const setFile = async (path) => { const doc = await send("DOM.getDocument", {}); const input = await send("DOM.querySelector", { nodeId: doc.root.nodeId, selector: "form[data-transcribe] input[type=file]" }); await send("DOM.setFileInputFiles", { nodeId: input.nodeId, files: [path] }); };
  const seen = async () => (await ev(`document.querySelectorAll("#lab-items .lab-item-transcribe").length`));

  const starts = await ev(`[...document.querySelectorAll("[data-transcribe-start] button")].map((b) => b.textContent.trim())`);
  check(starts.join() === "Record,Choose a file", `two equal starts: ${starts}`);

  if (voiceFile) {
    await setFile(voiceFile);
    const done = await trUntil((s) => !s.active && s.times > 0);
    check(done?.times === 3 && done.audio && /harbor was quiet/.test(done.text) && /English/.test(done.text), `a file starts by itself and comes back with times: ${done?.text.slice(0, 70)}`);
    await nav("/app/studio/transcribe");
    check((await trState())?.times === 3, "it is still there after leaving");
    await mouse("#lab-items .lab-item-transcribe [data-seek='1.2']");
    await sleep(300);
    const at = await ev(`${trItem}.querySelector("audio").currentTime`);
    check(at >= 1.2 && at < 2.5, `a time moves the player: ${at}`);
    check((await ev(`${trItem}.querySelector("[data-seek]").getAttribute("aria-label")`)) === "0:00, play from 0 seconds", "a time says what it does");
    await ev(`${trItem}.querySelector("[data-toggle-times]").click()`);
    check((await ev(`getComputedStyle(${trItem}.querySelector("[data-seek]")).display`)) === "none", "Times hides the times");
    await ev(`${trItem}.querySelector("[data-toggle-times]").click()`);
    const exports = await ev(`(async () => { const out = {}; for (const a of ${trItem}.querySelectorAll("a[download]")) { const r = await fetch(a.href); out[new URL(a.href).searchParams.get("format")] = [r.headers.get("content-disposition"), await r.text()]; } return out; })()`);
    check(/^1\n00:00:00,000 --> 00:00:01,200\nThe harbor/.test(exports.srt?.[1] ?? "") && exports.vtt?.[1].startsWith("WEBVTT\n\n00:00:00.000") && /harbor/.test(exports.txt?.[1] ?? "") && /transcript\.srt/.test(exports.srt?.[0] ?? ""), `exports are real SRT, VTT and text`);
    check((await ev(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)), "it fits the phone");

    // A player past its keep says so.
    await ev(`${trItem}.querySelector("audio").src = "/app/lab/results/nope"`);
    await sleep(500);
    check((await ev(`${trItem}.querySelector(".player-gone")?.textContent`)) === "Expired" && !(await ev(`!!${trItem}.querySelector("audio")`)), "a player past its keep says Expired");

    // A translation says so.
    await ev(`(() => { document.querySelector("form[data-transcribe] [name=translate]").checked = true; })()`);
    const before = await seen();
    await setFile(voiceFile);
    for (let i = 0; i < 60 && (await seen()) === before; i++) await sleep(150);
    const tr = await trUntil((s) => !s.active && /Translation/.test(s.text));
    check(/Translation/.test(tr?.text ?? "") && /Translated:/.test(tr?.text ?? "") && /Check a translation before relying on it/.test(tr?.text ?? ""), `a translation says so, and to check it: ${tr?.text.slice(0, 60)}`);
    await nav("/app/studio/transcribe");
  } else console.log("skip transcribing a file (set LBGO_VOICE_FILE)");

  // Before the browser's question is answered, the page says what it waits for.
  await ev(`(() => { const real = navigator.mediaDevices.getUserMedia; navigator.mediaDevices.getUserMedia = () => new Promise(() => {}); document.querySelector("[data-record-start]").click(); navigator.mediaDevices.getUserMedia = real; })()`);
  await sleep(100);
  check(/Allow the microphone/.test(await ev(`document.querySelector("[data-audio-status]").textContent`)), "waiting on the browser's question, it says so");
  await nav("/app/studio/transcribe");

  // An English-only model has nothing to choose.
  const englishBefore = await ev(`({ chip: document.querySelector("[data-open-sheet=lab-language]").hidden, note: document.querySelector("[data-english-note]").hidden })`);
  await ev(`(() => { document.querySelector("form[data-transcribe] input[type=hidden][name=model]").setAttribute("data-english-only", ""); document.dispatchEvent(new Event("htmx:afterSettle")); })()`);
  const englishAfter = await ev(`({ chip: document.querySelector("[data-open-sheet=lab-language]").hidden, note: document.querySelector("[data-english-note]").hidden })`);
  check(!englishBefore.chip && englishBefore.note && englishAfter.chip && !englishAfter.note, `an English-only model says so instead of offering languages: ${JSON.stringify(englishAfter)}`);
  await nav("/app/studio/transcribe");

  // Recording. This headless browser's own microphone never answers, so a generated
  // tone goes through the real recorder, level and sending. A real microphone is not
  // covered here.
  await ev(`navigator.mediaDevices.getUserMedia = async () => { const c = new AudioContext(); await c.resume(); const o = c.createOscillator(); const g = c.createGain(); g.gain.value = 0.3; const d = c.createMediaStreamDestination(); o.connect(g).connect(d); o.start(); return d.stream; }`);
  await mouse("[data-record-start]");
  for (let i = 0; i < 30 && (await ev(`document.querySelector("[data-record-panel]").hidden`)); i++) await sleep(100);
  await sleep(1500);
  const live = await ev(`({ panel: !document.querySelector("[data-record-panel]").hidden, start: document.querySelector("[data-transcribe-start]").hidden, time: document.querySelector("[data-record-time]").textContent, word: document.querySelector("[data-record-word]").textContent, level: parseFloat(document.querySelector("[data-record-level]").style.width), status: document.querySelector("[data-audio-status]").textContent })`);
  check(live.panel && live.start && live.time !== "0:00" && live.level >= 0 && /Hearing you|Too loud|Very quiet/.test(live.word), `recording shows a time, a level and a word: ${JSON.stringify(live)}`);
  await ev(`document.querySelector("a[href='/app/models']").click()`);
  await sleep(400);
  check(/Stop the recording first/.test(await ev(`document.querySelector("[data-audio-status]").textContent`)) && (await ev(`location.pathname`)) === "/app/studio/transcribe", "a link waits while recording");
  await ev(`document.querySelector("[data-record-pause]").click()`);
  const held = await ev(`document.querySelector("[data-record-time]").textContent`);
  await sleep(1300);
  const paused = await ev(`({ time: document.querySelector("[data-record-time]").textContent, word: document.querySelector("[data-record-word]").textContent, label: document.querySelector("[data-record-pause]").textContent })`);
  check(paused.time === held && paused.word === "Paused" && paused.label === "Resume", `Pause holds the time: ${JSON.stringify(paused)}`);
  await ev(`document.querySelector("[data-record-pause]").click()`);
  await sleep(300);
  const countBefore = await seen();
  await ev(`document.querySelector("[data-record-stop]").click()`);
  for (let i = 0; i < 80 && (await seen()) === countBefore; i++) await sleep(150);
  const rec = await trUntil((s) => !s.active && /Recording/.test(s.text));
  check(/Recording/.test(rec?.text ?? "") && rec.times === 3 && (await ev(`document.querySelector("[data-record-panel]").hidden`)), `Stop sends it by itself: ${rec?.text.slice(0, 60)}`);

  // A send that fails keeps the recording, even across a page change.
  await send("Network.enable");
  await mouse("[data-record-start]");
  for (let i = 0; i < 30 && (await ev(`document.querySelector("[data-record-panel]").hidden`)); i++) await sleep(100);
  await sleep(600);
  await send("Network.emulateNetworkConditions", { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
  const beforeLost = await seen();
  await ev(`document.querySelector("[data-record-stop]").click()`);
  for (let i = 0; i < 40 && (await ev(`document.querySelector("[data-keep]").hidden`)); i++) await sleep(150);
  const lost = await ev(`({ said: document.querySelector("[data-keep-said]").textContent, start: document.querySelector("[data-transcribe-start]").hidden })`);
  await send("Network.emulateNetworkConditions", { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
  check(/Your recording is still here, send again/.test(lost.said) && lost.start, `a failed send keeps the recording: ${JSON.stringify(lost)}`);
  // By its links, so the page lives on. Each one waits for the page it asked for, and says if it never came:
  // a slow link can otherwise leave the page where it was, and what follows would pass without moving at all.
  const visited = [];
  for (const href of ["/app/models", "/app/studio", "/app/studio/transcribe"]) {
    const clicked = await ev(`(() => { const a = document.querySelector("a[href='${href}']"); if (!a) return false; a.click(); return true; })()`);
    if (!clicked) { visited.push(`no link to ${href} on ${await ev("location.pathname")}`); break; }
    for (let i = 0; i < 80 && (await ev("location.pathname")) !== href; i++) await sleep(150);
    visited.push(await ev("location.pathname"));
    await sleep(300);
  }
  check(visited.join() === "/app/models,/app/studio,/app/studio/transcribe", `moving around the app goes through each page: ${visited}`);
  for (let i = 0; i < 40 && !(await ev(`!!document.querySelector("[data-keep]")`)); i++) await sleep(150);
  check(!(await ev(`document.querySelector("[data-keep]").hidden`)), "and it is still offered after moving around the app");
  await ev(`document.querySelector("[data-keep-send]").click()`);
  for (let i = 0; i < 60 && (await seen()) === beforeLost; i++) await sleep(150);
  const resent = await trUntil((s) => !s.active && s.times > 0);
  check((await seen()) === beforeLost + 1 && resent?.times === 3 && (await ev(`document.querySelector("[data-keep]").hidden`)), "Send again makes the transcript and clears it");

  // No microphone: it says how to fix it, and a file still works.
  await ev(`navigator.mediaDevices.getUserMedia = () => Promise.reject(new DOMException("no", "NotAllowedError"))`);
  await ev(`document.querySelector("[data-record-start]").click()`);
  await sleep(300);
  check(/site settings, or choose a file/.test(await ev(`document.querySelector("[data-audio-status]").textContent`)) && (await ev(`!document.querySelector("[data-transcribe-start]").hidden`)), "a refused microphone says how to fix it and leaves the file");
}

// --- Chat's strip and chip (DESIGN §25) -----------------------------------------
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/chat");
const stripNow = await ev(`(() => { const s = document.querySelector(".strip"); if (!s) return null; const r = s.getBoundingClientRect(); return { h: r.height, href: s.getAttribute("href"), font: parseFloat(getComputedStyle(s.querySelector(".strip-row")).fontSize), chip: !!document.querySelector("[data-open-picker] [data-model-name]") }; })()`);
check(stripNow && stripNow.h >= 44 && stripNow.font >= 12 && stripNow.href === "/app/models/running" && stripNow.chip, `the strip is at least 44 px tall, its words at least 12 px, it opens Running, and the model shows: ${JSON.stringify(stripNow)}`);
await ev(`document.querySelector("[data-suggest]")?.click()`);
check((await ev(`document.getElementById("chat-input").value.length > 0 && document.querySelectorAll("[data-messages] article").length === 0`)), "a suggestion starts a message and does not send it");

// --- A model that can never fit: its sentence stands, and nothing offers a retry --------------
await nav("/app/chat");
const tooLarge = "This model requires about 14.5 GiB of memory, but only 4.7 GiB is available for models on this host.";
await ev(`(() => {
  window.fetch = async () => new Response(JSON.stringify({ error: { code: "model_too_large", param: "model", message: ${JSON.stringify(tooLarge)} } }), { status: 422, headers: { "content-type": "application/json" } });
  const p = document.getElementById("chat-input"); p.value = "Hello"; p.dispatchEvent(new Event("input", { bubbles: true }));
  document.querySelector("#composer [data-send]").click();
})()`);
await sleep(800);
const refused = await ev(`(() => { const box = document.querySelector("[data-error-box]"); return { text: box.textContent, buttons: [...box.querySelectorAll("button")].map((b) => b.textContent) }; })()`);
check(refused.text.includes("too big for this computer") && refused.text.includes(tooLarge) && refused.buttons.join() === "Choose a model,Dismiss", `a model too large for this computer says so, keeps the gateway's sentence a tap away, and offers no retry: ${JSON.stringify(refused.buttons)}`);
check((await ev(`document.querySelector("[data-strip-word]")?.textContent`)) === "Not enough memory for this model", "the strip says the same as the refusal");
await sleep(3500);
check((await ev(`document.querySelector("[data-strip-word]")?.textContent`)) === "Not enough memory for this model", "the strip still says so after a refresh");
await ev(`[...document.querySelectorAll("[data-error-box] button")].find((b) => b.textContent === "Dismiss").click()`);
await sleep(3500);
check((await ev(`document.querySelector("[data-strip-word]")?.textContent`)) !== "Not enough memory for this model", "dismissing the refusal gives the strip back to real state");

// --- Decisions' prompt never covers what is above it (review of #410) --------------------------
await send("Emulation.setDeviceMetricsOverride", { width: 360, height: 640, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/decisions");
const dockClear = await ev(`(() => {
  const dock = document.querySelector("[data-docked]"), space = document.querySelector(".lab-dock-spacer");
  scrollTo(0, document.documentElement.scrollHeight);
  const before = space.previousElementSibling; let last = before;
  while (last && !last.getBoundingClientRect().height) last = last.previousElementSibling;
  return { dock: dock.getBoundingClientRect().height, space: space.getBoundingClientRect().height, lastBottom: last.getBoundingClientRect().bottom, dockTop: dock.getBoundingClientRect().top };
})()`);
check(dockClear.space >= dockClear.dock && dockClear.lastBottom <= dockClear.dockTop, `at 360 px the page scrolls clear of the Decisions prompt: ${JSON.stringify(dockClear)}`);

// --- With the keyboard up on a short phone, the words come first ------------------------------
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await send("Emulation.setTouchEmulationEnabled", { enabled: true });
await send("Emulation.setEmitTouchEventsForMouse", { enabled: true });
await nav("/app/chat");
await ev(`document.getElementById("chat-input").focus()`);
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 508, deviceScaleFactor: 2, mobile: true });
await sleep(700);
const typing = await ev(`(() => { const box = document.querySelector(".composer-box").getBoundingClientRect(), strip = document.querySelector(".strip"), r = strip.getBoundingClientRect(); return { flag: document.documentElement.hasAttribute("data-typing"), h: r.height, top: r.top, gauge: getComputedStyle(strip.querySelector(".gauge")).display, host: strip.querySelector(".strip-host b").textContent, fits: box.bottom <= innerHeight, over: document.documentElement.scrollWidth > document.documentElement.clientWidth }; })()`);
check(typing.flag && typing.h >= 44 && typing.h < 64 && typing.gauge !== "none" && typing.host && typing.fits && !typing.over, `keyboard up: the strip stays, with its gauge as a thin rule, and the composer stays in view: ${JSON.stringify(typing)}`);
if (process.env.UI_KEYBOARD_SHOT) await Bun.write(process.env.UI_KEYBOARD_SHOT, Buffer.from((await send("Page.captureScreenshot", { format: "png" })).data, "base64"));

await send("Emulation.setTouchEmulationEnabled", { enabled: false });
await send("Emulation.setEmitTouchEventsForMouse", { enabled: false });

// --- The smallest phone, empty: the heading is under the strip, not behind it -----------------
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 568, deviceScaleFactor: 2, mobile: true });
await nav("/app/chat");
const landing = await ev(`(() => { const strip = document.querySelector(".strip").getBoundingClientRect(), log = document.getElementById("chat-log"); log.scrollTop = 0; const h = document.querySelector(".chat-hello").getBoundingClientRect(), chip = document.querySelector("[data-model-name]"); return { strip: strip.bottom, hello: h.top, cut: chip.scrollWidth > chip.clientWidth }; })()`);
check(landing.hello >= landing.strip && !landing.cut, `at 320 px the heading starts below the strip and the model's name isn't cut: ${JSON.stringify(landing)}`);

// --- Transcribe: the prompt clears the last card; dismiss is a real target ----------------------
for (const [w, h] of [[390, 844], [1280, 800]]) {
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: w < 768 });
  await nav("/app/studio/transcribe");
  const cleared = await ev(`(() => {
    scrollTo(0, document.documentElement.scrollHeight);
    const items = [...document.querySelectorAll("#lab-items > li")], dock = document.querySelector("[data-docked]");
    if (!items.length || !dock) return { none: true };
    const small = [...document.querySelectorAll("#lab-items [hx-post$='/remove']")].map((b) => { const r = b.getBoundingClientRect(); return Math.min(r.width, r.height); }).filter((n) => n < 44);
    return { last: Math.round(items.at(-1).getBoundingClientRect().bottom), dock: Math.round(dock.getBoundingClientRect().top), small };
  })()`);
  check(cleared.none || (cleared.last <= cleared.dock && cleared.small.length === 0), `Transcribe at ${w}: the prompt clears the last card and dismiss is at least 44 px: ${JSON.stringify(cleared)}`);
}

// --- Running agrees with its own legend: a row for every runtime holding memory ----------------
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/models/running");
const shown = await ev(`(() => { const rows = [...document.querySelectorAll("#running-live .running-row")].map((r) => r.textContent.replace(/\\s+/g, " ").trim()); return { rows, buttons: document.querySelectorAll("#running-live [aria-label^='Free memory']").length, sheets: document.querySelectorAll("#running-live dialog[id^=unload-sheet]").length }; })()`);
check(shown.buttons === shown.rows.length && shown.sheets === shown.rows.length, `every row on Running has its own Free memory: ${JSON.stringify(shown.rows).slice(0, 300)}`);
for (const w of [320, 360, 390, 430, 480, 768]) {
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: 844, deviceScaleFactor: 2, mobile: w < 768 });
  await sleep(120);
  const inset = await ev(`[...document.querySelectorAll("#running-live .running-row")].map((li) => { const r = li.getBoundingClientRect(); return [...li.querySelectorAll(".running-actions > *")].map((el) => { const b = el.getBoundingClientRect(); return Math.round(Math.min(b.left - r.left, r.right - b.right, r.bottom - b.bottom)); }); }).flat()`);
  check(inset.every((n) => n >= 8), `at ${w}, a row's state and action stay inside its padding: ${JSON.stringify(inset)}`);
  const tallest = await ev(`Math.max(0, ...[...document.querySelectorAll(".running-actions .btn")].map((b) => b.getBoundingClientRect().height))`);
  check(tallest < 50, `at ${w}, Free memory stays on one line: ${tallest}`);
}

// --- An htmx target that isn't a valid selector does nothing, silently (a dot in a model's id).
await nav("/app/models/get");
const badTargets = await ev(`[...document.querySelectorAll("[hx-target^='#'], [hx-target^='.']")].map((el) => el.getAttribute("hx-target")).filter((t) => { try { document.querySelector(t); return false; } catch { return true; } })`);
check(badTargets.length === 0, `Get's targets are all valid selectors: ${JSON.stringify(badTargets).slice(0, 200)}`);

// --- Get finds its row for a model with a dot in its id. Stopped before the request, so nothing
// downloads. The second click plants the old target, to show this check would have caught it.
const clickGet = (plant) => ev(`(async () => {
  const form = [...document.querySelectorAll("form[hx-post='/app/models/get/recipe']")].find((f) => f.querySelector("[name=modelId]").value.includes("."));
  if (!form) return { none: true };
  const id = form.querySelector("[name=modelId]").value;
  if (${plant}) form.setAttribute("hx-target", "#cat-" + id + "-action");
  let requested = null;
  const stop = (e) => { requested = e.detail.target?.className ?? "?"; e.preventDefault(); };
  document.addEventListener("htmx:beforeRequest", stop, true);
  form.querySelector("button[type=submit]").click();
  await new Promise((r) => setTimeout(r, 400));
  document.removeEventListener("htmx:beforeRequest", stop, true);
  return { id, requested };
})()`);
const dotted = await clickGet(false);
check(dotted.id?.includes(".") && dotted.requested?.includes("catalog-action"), `Get on a dotted model id reaches its own row: ${JSON.stringify(dotted)}`);
await nav("/app/models/get");
const planted = await clickGet(true);
check(planted.id?.includes(".") && planted.requested === null, `the old target would not have sent anything: ${JSON.stringify(planted)}`);

// --- Sizes: targets at least 44 px and text at least 12 px, as rendered at 320 ----------------------
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 568, deviceScaleFactor: 2, mobile: true });
for (const path of ["/app/chat", "/app/studio", "/app/models", "/app/models/get", "/app/models/running", "/app/settings", "/app/welcome", "/app/welcome/choose", "/app/studio/image", "/app/studio/decisions"]) {
  await nav(path);
  const sizes = await ev(`(() => {
    const seen = (el) => { const r = el.getBoundingClientRect(), st = getComputedStyle(el); return r.width > 1 && r.height > 1 && st.visibility !== "hidden" && !el.closest(".sr-only, dialog:not([open]), [hidden]"); };
    const small = [...document.querySelectorAll("a.btn, button, input:not([type=hidden]):not([type=radio]):not([type=checkbox]), select, .chip, .tab, .list-row, .strip")].filter(seen).filter((el) => el.getBoundingClientRect().height < 43.5).map((el) => (el.getAttribute("aria-label") || el.textContent || el.tagName).trim().slice(0, 30) + " " + Math.round(el.getBoundingClientRect().height));
    const tiny = new Set();
    const walk = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    while (walk.nextNode()) { const n = walk.currentNode, el = n.parentElement; if (n.textContent.trim() && el && !["SCRIPT", "STYLE", "NOSCRIPT"].includes(el.tagName) && seen(el) && parseFloat(getComputedStyle(el).fontSize) < 11.99) tiny.add(n.textContent.trim().slice(0, 24) + " " + getComputedStyle(el).fontSize); }
    return { small, tiny: [...tiny] };
  })()`);
  check(sizes.small.length === 0 && sizes.tiny.length === 0, `at 320, ${path}: targets at least 44 px and text at least 12 px${sizes.small.length || sizes.tiny.length ? ": " + JSON.stringify(sizes).slice(0, 320) : ""}`);
  check(await ev(pageFits()), `at 320, ${path}: no sideways scroll`);
}

// --- The landing while a result is being made (DESIGN §27) ------------------------------------
// At 320, with several results, so the strip really scrolls: otherwise "keeps its place" proves nothing.
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 640, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/image");
if (await ev(`!!document.querySelector("#lab-prompt")`)) {
  for (const word of ["first", "second", "third"]) {
    const before = await ev(`document.querySelectorAll("#lab-items > li").length`);
    await ev(`(() => { document.querySelector("#lab-prompt").value = "a slow lighthouse, ${word}, fake:slow-image"; document.querySelector(".lab-dock button[type=submit]").click(); })()`);
    for (let i = 0; i < 30 && (await ev(`document.querySelectorAll("#lab-items > li").length`)) <= before; i++) await sleep(100);
  }
  await nav("/app/studio");
  const live = await ev(`(() => { const li = document.querySelector(".studio-recent > li"), s = document.querySelector(".studio-recent"); s.scrollLeft = 124; return { word: li.querySelector(".studio-tile-live")?.textContent.trim() ?? "", polls: li.hasAttribute("hx-get"), asking: document.querySelectorAll(".studio-recent > li[hx-get]").length, words: document.querySelectorAll(".studio-recent .studio-tile-live").length, left: s.scrollLeft, over: document.documentElement.scrollWidth > document.documentElement.clientWidth }; })()`);
  check(live.polls && live.left > 0 && live.asking === live.words && /…$/.test(live.word) && !live.over, `a result being made says how it stands, and asks for itself: ${JSON.stringify(live)}`);
  for (let i = 0; i < 100 && (await ev(`!!document.querySelector(".studio-recent > li[hx-get]")`)); i++) await sleep(150);
  await sleep(200);
  const settled = await ev(`(() => { const li = document.querySelector(".studio-recent > li"), s = document.querySelector(".studio-recent"); return { asking: document.querySelectorAll(".studio-recent > li[hx-get]").length, img: li.querySelector("img")?.naturalWidth > 0, left: s.scrollLeft }; })()`);
  check(settled.asking === 0 && settled.img && settled.left === live.left, `they settle by themselves and the strip keeps its place: ${JSON.stringify(settled)}`);
}

// --- The landing never scrolls sideways, with results in it (DESIGN §27) ------------------------
// The shelf has to be full and really scroll, or this proves nothing. clientWidth, not innerWidth:
// under mobile emulation innerWidth grows with an overflowing page, so comparing with it can't fail.
{
  const hasResults = await ev(`fetch("/app/studio").then((r) => r.text()).then((t) => (t.match(/class="studio-tile"/g) ?? []).length)`);
  if (hasResults < 4) console.log(`skip landing width (${hasResults} results)`);
  // The measure itself must be able to fail: a planted wide element has to read as overflow.
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio");
  const planted = await ev(`(async () => { const n = document.createElement("div"); n.style.cssText = "width:1200px;height:1px"; document.querySelector("main").append(n); await new Promise((r) => requestAnimationFrame(r)); const d = document.documentElement, caught = d.scrollWidth > d.clientWidth; n.remove(); return { caught, inner: innerWidth, client: d.clientWidth }; })()`);
  check(planted.caught, `the width measure catches a planted 1200 px element: ${JSON.stringify(planted)}`);
  if (hasResults < 4) console.log("skip the landing with results (fewer than four results)");
  if (hasResults >= 4) for (const [width, height] of [[320, 568], [360, 740], [390, 844], [430, 932], [768, 1024], [1280, 800]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: width < 1024 });
    await nav("/app/studio");
    const fit = await ev(`(() => { const d = document.documentElement, shelf = document.querySelector(".studio-recent"), right = (q) => Math.round(document.querySelector(q).getBoundingClientRect().right); return { scroll: d.scrollWidth, client: d.clientWidth, tiles: shelf.children.length, shelfScrolls: shelf.scrollWidth > shelf.clientWidth, strip: right(".strip"), list: right(".studio-tools .list-group") }; })()`);
    const inside = fit.scroll <= fit.client && fit.strip <= fit.client && fit.list <= fit.client;
    check(inside && fit.tiles >= 4 && (width > 430 || fit.shelfScrolls), `at ${width}, the landing with results stays on the screen and only its shelf scrolls: ${JSON.stringify(fit)}`);
  }
}

// --- The picture viewer inspects, frames and gives back your place (DESIGN §27.5) ----------------
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/image");
if ((await ev(`document.querySelectorAll(".image-open").length`)) < 2) console.log("skip the viewer (fewer than two pictures)");
if ((await ev(`document.querySelectorAll(".image-open").length`)) >= 2) {
  const inViewer = `(() => { const d = document.querySelector("dialog.viewer[open]"), st = d.querySelector(".viewer-stage"), b = d.querySelector("[data-view-zoom]"), m = d.querySelector("[data-view-mat]"), img = d.querySelector(".viewer-img"); return { id: d.id, zoomed: st.classList.contains("viewer-zoomed"), wider: st.scrollWidth > st.clientWidth, left: st.scrollLeft, pressed: b.getAttribute("aria-pressed"), label: b.getAttribute("aria-label"), mat: m.getAttribute("aria-pressed"), matLabel: m.getAttribute("aria-label"), ground: getComputedStyle(img).backgroundColor, hint: getComputedStyle(d.querySelector(".viewer-hint")).display }; })()`;
  const press = async (key, code, vk) => { for (const type of ["keyDown", "keyUp"]) await send("Input.dispatchKeyEvent", { type, key, code, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk }); await sleep(120); };
  await ev(`try { localStorage.removeItem("localbase.viewer.mat.v1") } catch {}`);
  await nav("/app/studio/image");
  const title = await ev(`(() => { const g = [...document.querySelectorAll("#lab-items > li")].find((n) => n.querySelector(".image-open")), t = g.querySelector(".image-title"), grid = g.querySelector(".image-grid"); return { above: t.getBoundingClientRect().bottom <= grid.getBoundingClientRect().top, font: getComputedStyle(t).fontFamily, keep: !!document.querySelector(".studio-keep") }; })()`);
  check(title.above && /Newsreader/.test(title.font) && title.keep, `a group leads with its words, and says how long pictures stay: ${JSON.stringify(title)}`);
  await ev(`document.querySelector(".image-open").click()`);
  await sleep(300);
  check((await ev(inViewer)).zoomed === false, "the viewer opens on the whole picture");
  await ev(`document.querySelector("dialog.viewer[open] [data-view-zoom]").click()`);
  await sleep(120);
  const zoomed = await ev(inViewer);
  check(zoomed.zoomed && zoomed.wider && zoomed.pressed === "true" && zoomed.label === "Zoom out" && zoomed.hint === "none", `Zoom in enlarges it, and says how to come back: ${JSON.stringify(zoomed)}`);
  await press("ArrowRight", "ArrowRight", 39);
  const panned = await ev(inViewer);
  check(panned.id === zoomed.id && panned.left !== zoomed.left, `zoomed in, the arrows move around the picture, not to the next one: ${JSON.stringify({ id: panned.id === zoomed.id, left: [zoomed.left, panned.left] })}`);
  await ev(`document.querySelector("dialog.viewer[open] .viewer-img").click()`);
  await sleep(120);
  check((await ev(inViewer)).zoomed === false, "a tap on the zoomed picture comes back");
  await press("z", "KeyZ", 90);
  check((await ev(inViewer)).zoomed === true, "Z zooms too");
  await ev(`document.querySelector("dialog.viewer[open] [data-view-step='1']").click()`);
  await sleep(200);
  const moved = await ev(inViewer);
  check(moved.id !== zoomed.id && moved.zoomed === false, `moving on shows the next whole picture: ${JSON.stringify(moved)}`);
  await ev(`document.querySelector("dialog.viewer[open] [data-view-mat]").click()`);
  await sleep(120);
  const matted = await ev(inViewer);
  check(matted.mat === "true" && matted.matLabel === "Show without a mat" && !/, 0\)$|transparent/.test(matted.ground), `the mat puts the picture on paper: ${JSON.stringify(matted)}`);
  await ev(`document.querySelector("dialog.viewer[open] [data-view-step='1']").click()`);
  await sleep(200);
  check((await ev(inViewer)).mat === "true", "the mat stays on for the next picture");
  await ev(`document.querySelector("dialog.viewer[open] [data-view-mat]").click()`);
  // The close event can take a moment to arrive, so this waits for the result, up to a deadline.
  const wentBack = await ev(`(async () => { const d = document.querySelector("dialog.viewer[open]"), id = d.id; d.close(); let focus = null; for (let i = 0; i < 30 && focus !== id; i++) { await new Promise((r) => setTimeout(r, 50)); focus = document.activeElement?.getAttribute?.("data-open-sheet") ?? null; } return { id, focus }; })()`);
  check(wentBack.focus === wentBack.id, `closing goes back to that picture, even after moving between them: ${JSON.stringify(wentBack)}`);
  // Details opens above the actions, across the bar, at every width: engines place a <details> box differently,
  // so this compares where the panel really is, on whichever browser this runs.
  for (const [width, height, mobile] of [[320, 568, true], [390, 844, true], [1280, 800, false]] as const) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: mobile ? 2 : 1, mobile });
    await nav("/app/studio/image");
    await ev(`document.querySelector(".image-open").click()`);
    await sleep(300);
    const panelOf = `document.querySelector("dialog.viewer[open] .viewer-panel")`;
    const closed = await ev(`${panelOf}.getClientRects().length`);
    await ev(`document.querySelector("dialog.viewer[open] [data-view-details]").click()`);
    await sleep(200);
    const placed = await ev(`(() => { const d = document.querySelector("dialog.viewer[open]"), foot = d.querySelector(".viewer-foot").getBoundingClientRect(), panel = ${panelOf}.getBoundingClientRect(), acts = [...d.querySelectorAll(".viewer-foot > .viewer-act")].filter((a) => a.getBoundingClientRect().width > 0).map((a) => a.getBoundingClientRect());
      return { width: Math.round(panel.width), foot: Math.round(foot.width), panelBottom: Math.round(panel.bottom), actsTop: Math.round(Math.min(...acts.map((a) => a.top))), rows: new Set(acts.map((a) => Math.round(a.top))).size, onScreen: Math.round(panel.top) >= 0 && Math.round(Math.max(...acts.map((a) => a.bottom))) <= innerHeight }; })()`);
    check(closed === 0 && placed.width >= placed.foot - 32 && placed.panelBottom <= placed.actsTop && placed.onScreen, `at ${width}, Details opens above the actions, across the bar: ${JSON.stringify({ closed, ...placed })}`);
    await ev(`document.querySelector("dialog.viewer[open] [data-view-details]").click()`);
    const shut = await ev(`({ gone: ${panelOf}.getClientRects().length === 0, said: document.querySelector("dialog.viewer[open] [data-view-details]").getAttribute("aria-expanded") })`);
    check(shut.gone && shut.said === "false", `at ${width}, Details closes again and says so: ${JSON.stringify(shut)}`);
    await ev(`document.querySelector("dialog.viewer[open]").close()`);
  }
  // On a small phone, with Share showing as it does on a real one, every action is on screen and big enough.
  await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 568, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/image");
  await ev(`document.querySelector(".image-open").click()`);
  await sleep(300);
  const small = await ev(`(() => { const d = document.querySelector("dialog.viewer[open]"); d.querySelector("[data-share]").hidden = false; const acts = [...d.querySelectorAll(".viewer-foot .viewer-act, .viewer-top .viewer-btn")].filter((n) => n.getBoundingClientRect().width > 1); const rects = acts.map((n) => n.getBoundingClientRect()); const img = d.querySelector(".viewer-img").getBoundingClientRect(); return { count: acts.length, inside: rects.every((r) => r.left >= 0 && r.right <= innerWidth && r.bottom <= innerHeight && r.top >= 0), big: rects.every((r) => r.width >= 43.5 && r.height >= 43.5), picture: Math.round(img.height) }; })()`);
  check(small.count >= 8 && small.inside && small.big && small.picture >= 150, `at 320, every viewer action fits and is big enough, and the picture stays: ${JSON.stringify(small)}`);
  await ev(`document.querySelector("dialog.viewer[open]").close()`);
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });

  // Edit (DESIGN §27.5): with a model that can, a picture goes into the prompt as the start of a new
  // one; without, it says what is needed. Which of these runs depends on the gateway's models.
  await nav("/app/studio/image");
  const openPicture = async () => { await ev(`document.querySelector(".image-open").click()`); await sleep(250); };
  if (!(await ev(`!!document.querySelector('form.lab-dock [name="model"][data-edits]')`))) {
    await openPicture();
    await ev(`document.querySelector("dialog.viewer[open] [data-edit-picture]").click()`);
    await sleep(250);
    const needs = await ev(`(() => { const d = document.getElementById("image-needs-edit"); return { open: !!d?.open, viewer: !!document.querySelector("dialog.viewer[open]"), link: d?.querySelector("a")?.getAttribute("href") ?? "", source: document.querySelector("form.lab-dock input[name=source]").value }; })()`);
    check(needs.open && needs.viewer && needs.link === "/app/models/get?filter=images" && needs.source === "", `with no model that can edit, Edit says what is needed: ${JSON.stringify(needs)}`);
    await ev(`document.getElementById("image-needs-edit").close(); document.querySelector("dialog.viewer[open]").close()`);
  } else {
    const dock = `(() => {
      const f = document.querySelector("form.lab-dock"), row = f.querySelector("[data-image-source]"), img = row.querySelector("img");
      const shown = (name) => { const c = f.querySelector(\`[data-chip-label=\${name}]\`)?.closest("button"); return !!c && c.getBoundingClientRect().width > 0; };
      const models = [...f.querySelectorAll("input[type=radio][name=model]")];
      return { row: row.getBoundingClientRect().height > 0, picture: img.naturalWidth > 0, source: f.elements.source.value, words: f.querySelector("#lab-prompt").value, hint: f.querySelector("#lab-prompt").placeholder,
        focus: document.activeElement?.id ?? "", viewer: !!document.querySelector("dialog.viewer[open]"), shape: shown("shape"), change: shown("change"), label: f.querySelector("[data-chip-label=change]").textContent,
        model: models.find((m) => m.checked)?.value ?? "", capable: models.find((m) => m.checked)?.hasAttribute("data-edits") ?? false, off: models.filter((m) => m.disabled).length, fits: document.documentElement.scrollWidth <= document.documentElement.clientWidth };
    })()`;
    // Starts on a model that can't, where there is one, so the switch and the way back are real.
    await ev(`(() => { const r = document.querySelector('form.lab-dock input[type=radio][name=model]:not([data-edits])'); if (r) { r.checked = true; r.dispatchEvent(new Event("change", { bubbles: true })); } })()`);
    const before = await ev(dock);
    const words = await ev(`document.querySelector(".image-open").closest(".image-run").querySelector(".image-title").textContent`);
    await openPicture();
    await ev(`document.querySelector("dialog.viewer[open] [data-edit-picture]").click()`);
    let on = await ev(dock);
    for (let i = 0; i < 30 && (on.focus !== "lab-prompt" || !on.picture); i++) { await sleep(100); on = await ev(dock); }
    check(on.row && on.picture && /^[0-9a-f]{32}$/.test(on.source) && !on.viewer, `Edit closes the viewer and puts the picture in the prompt: ${JSON.stringify(on)}`);
    check(on.focus === "lab-prompt" && on.words === words && on.hint === "Describe what to change", `Edit leaves its words ready to change: ${JSON.stringify(on)}`);
    await sleep(400);
    check((await ev(`document.activeElement?.id`)) === "lab-prompt", "closing the viewer for Edit doesn't take focus back to the picture");
    check(!on.shape && on.change && on.capable && on.fits, `while editing, the new picture keeps this one's shape, and only a model that can edit is chosen: ${JSON.stringify(on)}`);
    await ev(`document.querySelector("[data-source-clear]").click()`);
    await sleep(100);
    const off = await ev(dock);
    check(!off.row && off.source === "" && off.hint === "Describe the picture" && off.shape && !off.change && off.model === before.model && off.off === 0, `Stop editing puts everything back: ${JSON.stringify(off)}`);
    // On a wide screen the prompt is in the page's flow, so focusing it could move the page.
    await send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false });
    await nav("/app/studio/image");
    await openPicture();
    // Before and after: a reload can restore where an earlier step left the page.
    const was = await ev(`Math.round(document.scrollingElement.scrollTop)`);
    await ev(`document.querySelector("dialog.viewer[open] [data-edit-picture]").click()`);
    await sleep(400);
    const wide = await ev(`({ moved: Math.round(document.scrollingElement.scrollTop) - ${was}, focus: document.activeElement?.id, can: document.scrollingElement.scrollHeight > innerHeight + 100 })`);
    check(wide.can && wide.moved === 0 && wide.focus === "lab-prompt", `Edit doesn't move the page: ${JSON.stringify(wide)}`);
    await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
    await nav("/app/studio/image");
    await openPicture();
    await ev(`document.querySelector("dialog.viewer[open] [data-edit-picture]").click()`);
    await sleep(150);
    const runsBefore = (await runsNow()).length;
    await ev(`(() => { const f = document.querySelector("form.lab-dock"), little = f.querySelector("input[name=change][value=little]"); little.checked = true; little.dispatchEvent(new Event("change", { bubbles: true })); f.querySelector("#lab-prompt").value = "the same lighthouse in the snow"; f.querySelector("button[type=submit]").click(); })()`);
    const newest = `fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => { const list = b.data ?? b; return { count: list.length, target: list[0]?.target, status: list[0]?.status }; })`;
    let started = await ev(newest);
    for (let i = 0; i < 40 && (started.count <= runsBefore || started.status !== "completed"); i++) { await sleep(150); started = await ev(newest); }
    check(started.target === "/v1/images/edits" && started.status === "completed", `the edit runs on the edit route and finishes: ${JSON.stringify(started)}`);
    let made = "";
    for (let i = 0; i < 40 && !/^Edited/.test(made); i++) { await sleep(150); made = await ev(`document.querySelector("#lab-items > li .image-facts")?.textContent ?? ""`); }
    check(/^Edited/.test(made), `the new picture says it was edited: "${made}"`);
    const after = await ev(dock);
    check(!after.row && after.source === "" && after.shape, `once it has started, the next picture starts fresh: ${JSON.stringify(after)}`);
    await ev(`document.querySelector("#lab-items > li .image-open").click()`);
    await sleep(250);
    await ev(`document.querySelector("dialog.viewer[open] [data-view-details]").click()`);
    await sleep(200);
    const facts = await ev(`(() => { const d = document.querySelector("dialog.viewer[open]"), img = d.querySelector(".viewer-from"); return { text: d.querySelector(".viewer-facts").textContent, from: img?.naturalWidth > 0, again: [...d.querySelectorAll(".viewer-panel button")].some((b) => b.textContent.trim() === "Regenerate") }; })()`);
    check(/A little/.test(facts.text) && /Started from/.test(facts.text) && facts.from && facts.again, `Details say how it was edited and from what: ${JSON.stringify(facts)}`);
    const count = (await ev(newest)).count;
    await ev(`document.querySelector("dialog.viewer[open] [hx-vals*=vary]").click()`);
    let varied = await ev(newest);
    for (let i = 0; i < 40 && (varied.count <= count || varied.status !== "completed"); i++) { await sleep(150); varied = await ev(newest); }
    check(varied.count > count && varied.target === "/v1/images/edits", `Vary on an edit edits the same picture again: ${JSON.stringify(varied)}`);
    await nav("/app/studio/image");
  }
}

// --- A pill leads somewhere useful, and is never over content (DESIGN §27) -----------------------
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/image");
const imageModel = await ev(`document.querySelector("form input[name=model]")?.value ?? ""`);
if (!imageModel) console.log("skip the pill (no picture model)");
if (imageModel) {
  // Waits for what is running to end, then starts slow pictures, so the states below are really there.
  const anyActive = `fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => (b.data ?? b).some((x) => x.status === "queued" || x.status === "in_progress"))`;
  const settle = async () => { for (let i = 0; i < 120 && (await ev(anyActive)); i++) await sleep(150); };
  const startSlow = async () => {
    await settle();
    await ev(`(async () => { const t = JSON.parse(document.body.getAttribute("hx-headers") || "{}"); for (let i = 0; i < 3; i++) await fetch("/app/lab/image", { method: "POST", headers: { ...t, "HX-Request": "true", "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ prompt: "fake:slow-image a lighthouse " + i, count: "1", shape: "square", resolution: "standard", model: ${JSON.stringify(imageModel)} }) }); })()`);
  };
  const noPill = `[...document.querySelectorAll("[data-run-pill]")].every((n) => n.hidden)`;
  const reading = `(() => {
    const bar = document.querySelector(".run-pill-dock"), pill = document.querySelector("[data-run-pill=dock]"), r = bar.getBoundingClientRect();
    const words = [...document.querySelectorAll("main.page a, main.page button, main.page p, main.page li, main.page h2")].filter((n) => n.getBoundingClientRect().height > 0 && !n.closest("[data-docked], dialog"));
    const dock = document.querySelector("[data-docked]")?.getBoundingClientRect();
    return { shown: !pill.hidden && r.height > 0, href: pill.getAttribute("href"), top: Math.round(r.top), bottom: Math.round(r.bottom), ground: getComputedStyle(bar).backgroundColor, dockTop: dock ? Math.round(dock.top) : null, lastBottom: Math.round(words.at(-1)?.getBoundingClientRect().bottom ?? 0), over: document.documentElement.scrollWidth > document.documentElement.clientWidth };
  })()`;
  await startSlow();
  await nav("/app/studio/image");
  await sleep(250);
  check((await ev(anyActive)) && (await ev(noPill)), "no pill on the page that holds the result");
  await nav("/app/studio");
  await sleep(250);
  check((await ev(anyActive)) && (await ev(noPill)), "no pill on the landing for what Recent already shows");
  for (const [width, height] of [[320, 568], [360, 740], [390, 844]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    await nav("/app/models");
    await startSlow();
    await nav("/app/models");
    await sleep(250);
    const away = await ev(reading);
    check(away.shown && away.href === "/app/studio/image" && !/, 0\)$/.test(away.ground) && !away.ground.includes("transparent") && !away.over, `at ${width}, elsewhere it leads to the result, on its own ground: ${JSON.stringify(away)}`);
    await ev(`scrollTo(0, document.documentElement.scrollHeight)`);
    await sleep(120);
    const end = await ev(reading);
    check(end.shown && end.lastBottom <= end.top, `at ${width}, the page's last line clears it: ${JSON.stringify(end)}`);
    await startSlow();
    await nav("/app/studio/video");
    await sleep(250);
    const onTool = await ev(reading);
    check(onTool.shown && onTool.dockTop !== null && onTool.bottom <= onTool.dockTop + 1, `at ${width}, on a tool's page it sits above the prompt: ${JSON.stringify(onTool)}`);
    await settle();
  }
}

// --- The last thing on a page can be reached, with a run elsewhere (DESIGN §27) ------------------
// Scrolls to the end and asks the browser what is really at the last control's centre, with the
// pill showing and with the prompt where the page has one. A video keeps running for Image and
// Models; slow pictures do for Video, where a video's own run shows no pill.
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/video");
const canVideo = await ev(`!!document.querySelector("#lab-prompt") && !!document.querySelector("form input[name=model]")`);
const pictureModel = await (async () => { await nav("/app/studio/image"); return ev(`document.querySelector("form input[name=model]")?.value ?? ""`); })();
if (!(canVideo && pictureModel)) console.log("skip reaching the last control with a pill (needs a video and a picture model)");
if (canVideo && pictureModel) {
  const activeRuns = `fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => (b.data ?? b).some((x) => x.status === "queued" || x.status === "in_progress"))`;
  const lastControl = `(() => {
    scrollTo(0, document.documentElement.scrollHeight);
    const shown = [...document.querySelectorAll("main.page a[href], main.page button, main.page summary, main.page input:not([type=hidden]), main.page textarea, main.page select")].filter((n) => { const r = n.getBoundingClientRect(); return r.width > 1 && r.height > 1 && !n.closest("[data-docked], dialog:not([open]), [hidden], .sr-only") && getComputedStyle(n).visibility !== "hidden"; });
    const last = shown.at(-1), r = last.getBoundingClientRect(), x = r.left + r.width / 2, y = r.top + r.height / 2, at = document.elementFromPoint(x, y);
    const pill = document.querySelector("[data-run-pill=dock]");
    return { what: (last.getAttribute("aria-label") || last.textContent || last.tagName).trim().slice(0, 28), bottom: Math.round(r.bottom), height: innerHeight, reached: !!at && (at === last || last.contains(at) || at.contains(last)), at: at?.className?.toString().slice(0, 28) ?? null, pill: !pill.hidden && pill.getBoundingClientRect().height > 0 };
  })()`;
  await nav("/app/studio/video");
  await ev(`(() => { const p = document.getElementById("lab-prompt"); p.value = "fake:slow still going"; p.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector(".lab-dock button[type=submit]").click(); })()`);
  // A running job specifically: a finished video from earlier would pass at once, and leaving the page then cancels this request.
  for (let i = 0; i < 40 && !(await ev(`!!document.querySelector("#lab-items .lab-progress")`)); i++) await sleep(150);
  check(await ev(`!!document.querySelector("#lab-items .lab-progress")`), "a slow video is running for the checks below");
  for (const [width, height] of [[320, 568], [360, 740], [390, 844]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    for (const path of ["/app/studio/image", "/app/models"]) {
      await nav(path);
      for (let i = 0; i < 30 && (await ev(`document.querySelector("[data-run-pill=dock]").hidden`)); i++) await sleep(150);
      await sleep(150);
      const end = await ev(lastControl);
      check(end.pill && end.reached, `at ${width}, ${path}: the last control is reachable, with the pill showing: ${JSON.stringify(end)}`);
    }
  }
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/video");
  await ev(`document.querySelector('#lab-items [hx-post$="/cancel"]')?.click()`);
  for (let i = 0; i < 40 && (await ev(`!!document.querySelector('#lab-items [hx-post$="/cancel"]')`)); i++) await sleep(150);
  for (const [width, height] of [[320, 568], [360, 740], [390, 844]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    for (let i = 0; i < 120 && (await ev(activeRuns)); i++) await sleep(150);
    await nav("/app/studio/video");
    await ev(`(async () => { const t = JSON.parse(document.body.getAttribute("hx-headers") || "{}"); for (let i = 0; i < 3; i++) await fetch("/app/lab/image", { method: "POST", headers: { ...t, "HX-Request": "true", "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ prompt: "fake:slow-image a lighthouse " + i, count: "1", shape: "square", resolution: "standard", model: ${JSON.stringify(pictureModel)} }) }); })()`);
    await nav("/app/studio/video");
    for (let i = 0; i < 30 && (await ev(`document.querySelector("[data-run-pill=dock]").hidden`)); i++) await sleep(150);
    await sleep(150);
    const end = await ev(lastControl);
    check(end.pill && end.reached, `at ${width}, /app/studio/video: the last control is reachable, with the pill showing: ${JSON.stringify(end)}`);
  }
  for (let i = 0; i < 120 && (await ev(activeRuns)); i++) await sleep(150);
}

// --- A finished video, its header and its actions on one phone screen (DESIGN §27) ----------------
const settleVideoPage = async () => { for (let i = 0; i < 40 && !(await ev(`!!document.querySelector("#lab-items > li .video-actions")`)); i++) await sleep(150); };
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio/video");
if (!(await ev(`!!document.querySelector("form[aria-label='Make a video']")`))) console.log("skip finished video (no video model selected)");
else {
  // This section makes its own: earlier steps have removed theirs by now, and for a while it skipped without saying so.
  await ev(`(() => { const p = document.getElementById("lab-prompt"); p.value = "a quiet bay at dawn"; document.querySelector("form[aria-label='Make a video'] button[type=submit]").click(); })()`);
  for (let i = 0; i < 160 && !(await ev(`!!document.querySelector("#lab-items video")`)); i++) await sleep(150);
  check(await ev(`!!document.querySelector("#lab-items video")`), "a finished video is here for the checks below");
  // The shortest phone can't hold it all on one screen (DESIGN §27.2a), so there a scroll has to reach the actions, uncovered.
  await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 568, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/video");
  const shortest = await ev(`(async () => { const item = [...document.querySelectorAll("#lab-items > li")].find((n) => n.querySelector("video")), foot = item.querySelector(".lab-item-foot"), dock = document.querySelector("[data-docked]").getBoundingClientRect(); scrollTo(0, 0); const first = Math.round(foot.getBoundingClientRect().bottom); scrollBy(0, foot.getBoundingClientRect().bottom - dock.top + 12); await new Promise((r) => setTimeout(r, 150)); const r = foot.getBoundingClientRect(), a = foot.querySelector("a, button"), ar = a.getBoundingClientRect(), at = document.elementFromPoint(ar.left + ar.width / 2, ar.top + ar.height / 2); return { first, dockTop: Math.round(dock.top), after: Math.round(r.bottom), reached: !!at && (at === a || a.contains(at)), player: Math.round(item.querySelector("video").getBoundingClientRect().height) }; })()`);
  check(shortest.after <= shortest.dockTop && shortest.reached && shortest.player >= 100, `at 320 by 568, a scroll reaches a video's actions, uncovered: ${JSON.stringify(shortest)}`);
  for (const [width, height] of [[360, 740], [390, 844], [430, 932]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    await nav("/app/studio/video");
    // With Share showing, as it does on a real phone. On a browser without it, #483's row fitted and on one with it, it wrapped.
    const fit = await ev(`(() => { scrollTo(0, 0); const item = [...document.querySelectorAll("#lab-items > li")].find((n) => n.querySelector("video")); item.querySelector("[data-share]").hidden = false; const v = item.querySelector("video").getBoundingClientRect(), foot = item.querySelector(".lab-item-foot").getBoundingClientRect(), head = item.querySelector(".image-title").getBoundingClientRect(), dock = document.querySelector("[data-docked]").getBoundingClientRect(); return { head: Math.round(head.top), player: Math.round(v.height), actionsEnd: Math.round(foot.bottom), dockTop: Math.round(dock.top), share: +(v.height / innerHeight).toFixed(2), width: Math.round(v.width), fit: getComputedStyle(item.querySelector("video")).objectFit }; })()`);
    check(fit.head >= 0 && fit.actionsEnd <= fit.dockTop && fit.player >= 100 && fit.fit === "contain", `at ${width}, a video's heading, player and actions are all on one screen: ${JSON.stringify(fit)}`);
  }
  // A long title and a pill together are the tightest case: two lines of words, and another tool's run showing.
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await nav("/app/studio/video");
  const long = "A long slow pan across a quiet harbour at first light, with gulls overhead and small boats rocking on the water";
  await ev(`(() => { const p = document.getElementById("lab-prompt"); p.value = ${JSON.stringify(long)}; document.querySelector(".lab-dock button[type=submit]").click(); })()`);
  // That video itself, not an older one further down: the title shows as soon as it starts.
  const longDone = `(() => { const item = document.querySelector("#lab-items > li"); return !!item?.querySelector("video") && !!item.querySelector("[data-share]") && !!item.querySelector(".image-title")?.textContent.startsWith("A long slow pan"); })()`;
  for (let i = 0; i < 200 && !(await ev(longDone)); i++) await sleep(150);
  check(await ev(longDone), "the video with the long title has finished, so its actions are there to read");
  const slowPictures = `(async () => { const t = JSON.parse(document.body.getAttribute("hx-headers") || "{}"); for (let i = 0; i < 3; i++) await fetch("/app/lab/image", { method: "POST", headers: { ...t, "HX-Request": "true", "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ prompt: "fake:slow-image a slow lighthouse " + i, count: "1", shape: "square", resolution: "standard", model: "dreamshaper-8-lcm" }) }); })()`;
  for (const [width, height] of [[360, 740], [390, 844]]) {
    await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: true });
    await nav("/app/studio/video");
    await ev(slowPictures);
    const pillOn = `(() => { const p = document.querySelector("[data-run-pill=dock]"); return !!p && !p.hidden && p.getBoundingClientRect().height > 0; })()`;
    for (let i = 0; i < 100 && !(await ev(pillOn)); i++) await sleep(150);
    const tight = await ev(`(() => { scrollTo(0, 0); const item = document.querySelector("#lab-items > li"), title = item.querySelector(".image-title"), share = item.querySelector("[data-share]"), bar = document.querySelector(".run-pill-dock").getBoundingClientRect(), line = parseFloat(getComputedStyle(title).lineHeight); share.hidden = false;
      const acts = [...item.querySelectorAll(".lab-item-foot a, .lab-item-foot button")].filter((a) => a.getBoundingClientRect().width > 0), reach = (a) => { const r = a.getBoundingClientRect(), at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return !!at && (at === a || a.contains(at)); };
      return { lines: Math.round(title.getBoundingClientRect().height / line), acts: acts.length, rows: new Set(acts.map((a) => Math.round(a.getBoundingClientRect().top))).size, bottom: Math.round(Math.max(...acts.map((a) => a.getBoundingClientRect().bottom))), barTop: Math.round(bar.top), reached: acts.every(reach), pill: ${pillOn}, player: Math.round(item.querySelector("video").getBoundingClientRect().height) }; })()`);
    check(tight.pill && tight.lines === 2 && tight.acts >= 5 && tight.rows === 1 && tight.bottom <= tight.barTop && tight.reached && tight.player >= 100, `at ${width}, with a two-line title, a pill and Share showing, every action is in one row and none is covered: ${JSON.stringify(tight)}`);
    const left = await ev(`fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => (b.data ?? b).filter((x) => x.status === "queued" || x.status === "in_progress").map((x) => x.id))`);
    for (const id of left ?? []) await ev(`fetch("/app/api/_localbase/runs/${id}/cancel", { method: "POST", headers: { "x-localbase-ui": "1" } }).then((r) => r.status)`);
  }
  await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  // The icons, for a thumb: big enough, named for a screen reader, a long press shows the name and does nothing
  // else, and a tap still works. A first visit teaches the pencil, which a tooltip can't on a phone.
  await send("Emulation.setTouchEmulationEnabled", { enabled: true });
  await nav("/app/studio/video");
  await settleVideoPage();
  const pencil = `document.querySelector("#lab-items > li .video-actions [aria-label='Edit prompt']")`;
  const removeBtn = `document.querySelector("#lab-items > li .video-actions [aria-label='Remove this video']")`;
  const icons = await ev(`[...document.querySelectorAll("#lab-items > li .video-actions .icon-btn")].filter((b) => b.getBoundingClientRect().width > 0).map((b) => { const r = b.getBoundingClientRect(); return { name: b.getAttribute("aria-label"), w: Math.round(r.width), h: Math.round(r.height) }; })`);
  check(icons.length >= 3 && icons.every((i) => i.w >= 44 && i.h >= 44 && i.name) && icons.some((i) => i.name === "Edit prompt"), `the video's icons are 44 px and named: ${JSON.stringify(icons)}`);
  const centre = async (sel) => ev(`(() => { const r = ${sel}.getBoundingClientRect(); return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) }; })()`);
  const count = `document.querySelectorAll("#lab-items > li").length`;
  const itemsBefore = await ev(count);
  const at = await centre(removeBtn);
  await send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [at] });
  await sleep(650);
  const label = await ev(`document.querySelector(".icon-name")?.textContent ?? ""`);
  await send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await sleep(1300);
  const held = await ev(`({ items: ${count}, left: !!document.querySelector(".icon-name") })`);
  check(label === "Remove" && held.items === itemsBefore && !held.left, `a long press on Remove shows its name and removes nothing: ${JSON.stringify({ label, itemsBefore, ...held })}`);
  await ev(`(() => { document.getElementById("lab-prompt").value = ""; })()`);
  const tapAt = await centre(pencil);
  await send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [tapAt] });
  await send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await sleep(500);
  const tapped = await ev(`({ words: document.getElementById("lab-prompt").value.length, label: !!document.querySelector(".icon-name") })`);
  check(tapped.words > 0 && !tapped.label, `a tap on the pencil still puts the words back, with no label: ${JSON.stringify(tapped)}`);
  await send("Emulation.setTouchEmulationEnabled", { enabled: false });
}

// A hidden tab would have made the checks above mean something else, so say if it happened.
check(!(await ev(`document.hidden`)), "the page stayed visible through the run");

// --- The strip's words, under long names (review of #410) ---------------------------------
await send("Emulation.setDeviceMetricsOverride", { width: 320, height: 640, deviceScaleFactor: 2, mobile: true });
await nav("/app/studio");
for (const name of ["Workshop Mac Studio in Attic ", "工房のマックスタジオ工房のマックスタジオ工房のマックスタジオ"]) {
  const fits = await ev(`(() => {
    const b = document.querySelector(".strip-host b"), w = document.querySelector(".strip-word"), s = document.querySelector(".strip");
    b.textContent = ${JSON.stringify(name)}; w.textContent = "Getting the model ready";
    return { over: document.documentElement.scrollWidth > document.documentElement.clientWidth, cut: w.scrollWidth > w.clientWidth, inside: w.getBoundingClientRect().right <= s.getBoundingClientRect().right, title: !!b.title, ell: getComputedStyle(b).textOverflow };
  })()`);
  check(!fits.over && !fits.cut && fits.inside && fits.title && fits.ell === "ellipsis", `at 320 px a long name gives way and the state stays whole: ${JSON.stringify(fits)}`);
}

// --- Control boundaries and selection marks, both themes, from rendered colors (DESIGN §25) --
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
const measure = `(() => {
  const rgb = (c) => c.match(/[\\d.]+/g).map(Number);
  const lin = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
  const L = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
  const behind = (el) => { for (let e = el; e; e = e.parentElement) { const c = rgb(getComputedStyle(e).backgroundColor); if (c.length < 4 || c[3] > 0) return c; } return [255, 255, 255]; };
  const ratio = (a, b) => { const x = L(a), y = L(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const out = [];
  const seen = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  for (const el of document.querySelectorAll(".composer-box, .strip, .input, .chip:not([aria-current=true]), .btn-quiet")) {
    if (!seen(el) || parseFloat(getComputedStyle(el).borderTopWidth) === 0) continue;
    const n = ratio(rgb(getComputedStyle(el).borderTopColor), behind(el.parentElement));
    if (n < 3) out.push(el.className.split(" ")[0] + " " + n.toFixed(2));
  }
  for (const [sel, side] of [[".tab[aria-current=page]", "Top"], [".segmented > a[aria-current=page]", "Top"], [".nav-item[aria-current=page]", "Left"], [".decide-types label:has(:checked)", "Top"]]) {
    for (const el of document.querySelectorAll(sel)) {
      if (!seen(el)) continue;
      const n = ratio(rgb(getComputedStyle(el)["border" + side + "Color"]), behind(el));
      if (n < 3) out.push(sel + " " + n.toFixed(2));
    }
  }
  for (const el of document.querySelectorAll(".chip[aria-current=true]")) {
    if (!seen(el)) continue;
    const n = ratio(rgb(getComputedStyle(el).backgroundColor), behind(el.parentElement));
    if (n < 3) out.push("chip selected " + n.toFixed(2));
  }
  return out;
})()`;
for (const scheme of ["light", "dark"]) {
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
  for (const [w, h] of [[390, 844], [1280, 800]]) {
    await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: w < 768 });
    for (const path of ["/app/chat", "/app/studio", "/app/models", "/app/models/running", "/app/settings", "/app/studio/image", "/app/studio/decisions"]) {
      await nav(path);
      const low = await ev(measure);
      check(low.length === 0, `${scheme} ${w}: boundaries and selection marks on ${path} are at least 3:1${low.length ? ": " + low.join(", ") : ""}`);
    }
  }
}
await send("Emulation.setEmulatedMedia", { features: [] });

ws.close();
console.log(fails ? `e2e: ${fails} failure(s)` : "e2e: all passed");
process.exit(fails ? 1 : 0);
