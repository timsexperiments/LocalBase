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
await nav("/app/lab");
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
ws.close();
console.log(fails ? `e2e: ${fails} failure(s)` : "e2e: all passed");
process.exit(fails ? 1 : 0);
