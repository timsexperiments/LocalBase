// Drives headless Chrome for tools/chat-runs-check.sh: detached chat runs
// end to end (leave and return, the pill, Stop, the draft, a dropped
// connection). Exits 1 when any check fails.
const [cdpBase, base] = process.argv.slice(2);
const target = await (await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let id = 0; const waiting = new Map();
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.id && waiting.has(m.id)) { waiting.get(m.id)(m); waiting.delete(m.id); } });
const send = (method, params = {}) => new Promise((res, rej) => { const n = ++id; waiting.set(n, (m) => m.error ? rej(new Error(method + m.error.message)) : res(m.result)); ws.send(JSON.stringify({ id: n, method, params })); });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ev = async (expression) => { const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }); if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 400)); return r.result.value; };
const nav = async (path) => { await send("Page.navigate", { url: base + path }); await sleep(1500); };
const runsNow = () => ev(`fetch("/app/api/_localbase/runs",{headers:{"x-localbase-ui":"1"}}).then(r=>r.json()).then(b=>b.data.map(r=>r.status+":"+r.label))`);
let fails = 0; const check = (ok, msg) => { console.log((ok ? "ok   " : "FAIL ") + msg); if (!ok) fails++; };
await send("Page.enable");
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
await sleep(7000); // two polls with no change
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
  check(await ev(`!!document.querySelector(".studio-recent a[href='/app/studio/image']")`), "the landing's Recent shows it");
  check(await ev(`document.querySelector(".studio-recent img")?.naturalWidth > 0`), "a real picture decodes");
  // A file that is gone shows the drawing and "Expired", never a broken icon.
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
  check(view.full && view.img && view.tile && view.small === 0 && /of/.test(view.count) && ["Save", "Vary", "Regenerate", "Details"].every((a) => view.acts.includes(a)), `the viewer fills the screen with its actions: ${JSON.stringify(view)}`);
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
await ev(`void fetch("/app/api/v1/chat/completions", { method: "POST", headers: { "content-type": "application/json", "x-localbase-ui": "1" }, body: JSON.stringify({ model: "qwen3.5-4b-q4_k_m", stream: true, messages: [{ role: "user", content: "fake:slow 160" }] }) })`);
for (let i = 0; i < 30 && !(await ev(`!!document.querySelector("#running-live .pill-warn")`)); i++) await sleep(250);
await ev(`document.querySelector("#running-live [aria-label^='Unload']").click()`);
await sleep(300);
const asked = await ev(`({ open: document.getElementById("unload-sheet")?.open, text: document.getElementById("unload-sheet")?.textContent.replace(/\\s+/g, " ") })`);
check(asked.open && /This frees about .* Anything running on it finishes first/.test(asked.text), `Unload asks first: ${JSON.stringify(asked)}`);
await sleep(3500); // a poll would have replaced the sheet; it waits while it is open
check(await ev(`document.getElementById("unload-sheet")?.open === true`), "the sheet survives polling");
await ev(`document.querySelector("#unload-sheet button.btn-primary").click()`);
await sleep(400);
const waitingText = await ev(`(() => { const b = document.getElementById("unload-busy"); return b && getComputedStyle(b).display !== "none" ? b.textContent.trim() : ""; })()`);
check(/^Unloading…/.test(waitingText), `while draining it says so: ${JSON.stringify(waitingText)}`);
for (let i = 0; i < 40 && !(await ev(`document.getElementById("running-live").textContent.includes("Unloaded")`)); i++) await sleep(250);
const unloaded = await ev(`({ shown: document.getElementById("running-live").textContent.includes("Unloaded"), said: document.getElementById("running-status").textContent, empty: document.getElementById("running-live").textContent.includes("Nothing loaded") })`);
check(unloaded.shown && /^Unloaded/.test(unloaded.said) && unloaded.empty, `unloaded, shown and announced: ${JSON.stringify(unloaded)}`);
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
}
// --- Decisions (DESIGN §22.17) --------------------------------------------------
await nav("/app/studio/decisions");
if (!(await ev(`!!document.getElementById("decide")`))) {
  console.log("skip decisions (no decision model selected)");
} else {
  const typeTop = `document.querySelector(".decide-types").getBoundingClientRect().top`;
  const pick = (id) => `(async () => { document.querySelector("[data-open-sheet=decide-template]").click(); await new Promise((r) => setTimeout(r, 50)); document.querySelector('#decide-template [data-template="${id}"]').click(); })()`;
  const situation = (text) => `(() => { const a = document.getElementById("decide-situation"); a.value = ${JSON.stringify(text)}; a.dispatchEvent(new Event("input", { bubbles: true })); })()`;
  const send = `document.querySelector("#decide button[type=submit]").click()`;
  const finished = async () => { for (let i = 0; i < 60 && (await ev(`!document.querySelector("[data-result] .decide-word") && !document.querySelector("[data-result] [role=alert]")`)); i++) await sleep(100); };
  const read = `({ word: document.querySelector("[data-result] .decide-word")?.textContent, verdict: document.querySelector("[data-result] .decide-verdict")?.textContent, labels: [...document.querySelectorAll("[data-result] .decide-bar-label")].map((n) => n.firstChild.textContent), said: document.getElementById("decide-said").textContent })`;
  const decisionRuns = async () => (await ev(`fetch("/app/api/_localbase/runs", { headers: { "x-localbase-ui": "1" } }).then((r) => r.json()).then((b) => (b.data ?? b).filter((x) => x.label === "Decision").map((x) => x.status))`));

  const top = await ev(typeTop);
  await ev(pick("route"));
  check((await decisionRuns()).length === 0 && (await ev(`document.getElementById("lab-prompt").value`)).includes("team") && !(await ev(`document.querySelector("[data-result]").hidden === false`)), "a template fills the form and runs nothing");

  await ev(`document.querySelector("input[name=decide-type][value=noul]").click()`);
  check((await ev(typeTop)) === top && (await ev(`document.querySelector("[data-options]").hidden`)), "the question type stays put; yes or no needs no options");
  await ev(`document.querySelector("input[name=decide-type][value=choice]").click()`);

  await ev(send);
  await finished();
  const strong = await ev(read);
  check(strong.word === "Strongly favored" && /^billing/.test(strong.verdict) && strong.labels[0] === "billing" && /Strongly favored: billing, 91%/.test(strong.said), `words first, then ranked odds, and announced: ${JSON.stringify(strong)}`);
  check((await ev(`document.querySelector("[data-result]").textContent.includes("not a check that it is right")`)), "the standing line is under the answer");
  check((await ev(`document.querySelector("[data-form-open]").hidden && !document.querySelector("[data-form-summary]").hidden`)) && (await ev(typeTop)) === top, "the form collapses to a summary, and the type did not move");
  await sleep(200);
  check((await decisionRuns()).length === 0, "the run is deleted once its answer is here");

  await ev(`document.querySelector("[data-code]:not([data-form-open] *)").click()`);
  const code = await ev(`({ open: document.getElementById("decide-code").open, text: document.querySelector("[data-code-text]").textContent })`);
  check(code.open && code.text.includes("$LOCALBASE_API_KEY") && code.text.includes("/v1/systemone") && code.text.includes("billing"), "View code shows the request with the key as a placeholder");
  await ev(`document.querySelector("input[name=decide-code-lang][value=js]").click()`);
  check((await ev(`document.querySelector("[data-code-text]").textContent.includes("process.env.LOCALBASE_API_KEY")`)), "and JavaScript");
  await ev(`document.getElementById("decide-code").close()`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:close it could be either"));
  await ev(send);
  await sleep(300); await finished();
  const close = await ev(read);
  check(close.word === "Close call" && /^billing or shipping/.test(close.verdict), `a close call names both: ${close.verdict}`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:unsure"));
  await ev(send);
  await sleep(300); await finished();
  check((await ev(read)).word === "Unsure", "no answer past half reads Unsure");

  await ev(pick("urgent"));
  await ev(send);
  await sleep(300); await finished();
  const scale = await ev(read);
  check(scale.labels.join() === "low,medium,high" && /^low/.test(scale.verdict), `a scale keeps its order: ${scale.labels}`);

  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(`(() => { document.querySelector("input[name=decide-type][value=choice]").click(); for (const n of document.querySelectorAll("[data-remove-option]")) document.querySelector("[data-remove-option]")?.click(); })()`);
  await ev(send);
  const none = await ev(`({ error: document.getElementById("decide-error").textContent, q: document.getElementById("lab-prompt").value })`);
  check(/at least two options/.test(none.error) && none.q.length > 0, `too few options: says so and keeps the question: ${none.error}`);

  // Stop, and finding a run again after a reload.
  await ev(pick("route"));
  await ev(situation("fake:slow one"));
  await ev(send);
  await sleep(300);
  check((await ev(`!!document.querySelector("[data-result] [aria-busy] .skeleton") && !!document.querySelector("[data-stop]")`)), "while deciding: its place, and Stop");
  await ev(`document.querySelector("[data-stop]").click()`);
  await sleep(400);
  check((await ev(`document.querySelector("[data-result]").hidden && !document.querySelector("[data-form-open]").hidden`)) && (await ev(`document.getElementById("decide-situation").value`)).includes("fake:slow"), "Stop returns to the form with its text");
  await ev(send);
  await sleep(400);
  await nav("/app/studio/decisions");
  check((await ev(`!!document.querySelector("[data-result] [aria-busy]")`)) || (await ev(`!!document.querySelector("[data-result] .decide-word")`)), "a reload finds the question still running");
  await sleep(1000);
  for (let i = 0; i < 60 && !(await ev(`!!document.querySelector("[data-result] .decide-word")`)); i++) await sleep(100);
  check((await ev(read)).word === "Strongly favored", "and then its answer");

  // Saved examples stay in this browser.
  await ev(`document.querySelector("[data-save]").click()`);
  await nav("/app/studio/decisions");
  await ev(`document.querySelector("[data-open-sheet=decide-template]").click()`);
  check((await ev(`document.querySelector("#decide-template [data-saved] [data-template]")?.textContent.includes("Which team")`)), "a saved example is there after a reload");
  await ev(`document.querySelector("#decide-template [data-unsave]").click()`);
  check((await ev(`document.querySelector("[data-saved-box]").hidden`)), "and can be deleted");
  await ev(`document.getElementById("decide-template").close()`);

  // A batch.
  await ev(pick("route"));
  await ev(`document.querySelector("[data-batch-chip]").click()`);
  await ev(situation("one\nfake:close two\nfake:unsure three\n=SUM(1)\nfive"));
  check((await ev(`document.querySelector("[data-situation-count]").textContent`)) === "5 situations", "a batch counts its lines");
  await ev(send);
  for (let i = 0; i < 80 && (await ev(`!/5 of 5/.test(document.querySelector("[data-batch-summary]")?.textContent ?? "")`)); i++) await sleep(100);
  const summary = await ev(`document.querySelector("[data-batch-summary]").textContent`);
  check(summary === "5 of 5 done, 2 to review", `the batch says how it went: ${summary}`);
  await ev(`document.querySelector("input[name=decide-filter][value=review]").click()`);
  const visible = await ev(`[...document.querySelectorAll(".decide-row:not(.decide-row-head)")].filter((n) => !n.hidden).map((n) => n.querySelector(".decide-word").textContent)`);
  check(visible.join() === "Close call,Unsure", `the filter leaves close calls and unsure: ${visible}`);
  await ev(`document.querySelector("input[name=decide-filter][value=all]").click()`);
  check((await ev(`document.documentElement.scrollWidth <= innerWidth`)), "the batch fits the phone");
  await ev(`document.querySelector("[data-edit]").click()`);
  await ev(situation("fake:slow a\nfake:slow b\nfake:slow c"));
  await ev(send);
  await sleep(3600);
  await ev(`document.querySelector("[data-batch-stop]").click()`);
  await sleep(500);
  const stopped = await ev(`({ s: document.querySelector("[data-batch-summary]").textContent, more: !document.querySelector("[data-batch-continue]").hidden })`);
  check(/^1 of 3 done/.test(stopped.s) && stopped.more, `Stop keeps finished rows and offers Continue: ${JSON.stringify(stopped)}`);
  await ev(`document.querySelector("[data-batch-chip]").click()`);
  await ev(`sessionStorage.removeItem("localbase.decisions.v1")`);
}

ws.close();
console.log(fails ? `e2e: ${fails} failure(s)` : "e2e: all passed");
process.exit(fails ? 1 : 0);
