// Drives headless Chrome over the DevTools protocol for tools/details-check.sh.
// Opens every <details> on the pages people reach and records where its content lands, for compare.ts.
const [cdp, base, out] = process.argv.slice(2);
const t = await (await fetch(`${cdp}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(t.webSocketDebuggerUrl); await new Promise((r) => ws.addEventListener("open", r, { once: true }));
// With its browser gone nothing here can finish: stop rather than wait on it.
ws.addEventListener("close", () => process.exit(2));
let id = 0; const w = new Map();
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.id) { w.get(m.id)?.(m); w.delete(m.id); } });
const send = (method: string, params: any = {}) => new Promise<any>((r) => { const i = ++id; w.set(i, (m: any) => r(m.result)); ws.send(JSON.stringify({ id: i, method, params })); });
const ev = async (expression: string) => (await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }))?.result?.value;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
await send("Page.enable");
await send("Emulation.setFocusEmulationEnabled", { enabled: true });
const version = (await (await fetch(`${cdp}/json/version`)).json()).Browser;
const openViewer = `(async () => { document.querySelector(".image-open")?.click(); await new Promise((r) => setTimeout(r, 300)); document.querySelector("dialog.viewer[open] [data-view-details]")?.click(); await new Promise((r) => setTimeout(r, 200)); })()`;
const sheetOf = (sel: string) => `document.querySelector(${JSON.stringify(sel)})?.click()`;
const PAGES: [string, string, string][] = [
  ["settings", "/app/settings", ""], ["running", "/app/models/running", ""], ["running, free memory sheet", "/app/models/running", sheetOf("#running-live [data-open-sheet='unload-sheet']")],
  ["studio", "/app/studio", ""], ["image", "/app/studio/image", ""], ["image viewer, Details", "/app/studio/image", openViewer], ["video", "/app/studio/video", ""], ["speech", "/app/studio/speech", ""], ["transcribe", "/app/studio/transcribe", ""], ["decisions", "/app/studio/decisions", ""],
  ["models", "/app/models", ""], ["get models", "/app/models/get", ""], ["connect", "/app/connect", ""], ["chat", "/app/chat", ""], ["chat, a reply with reasoning", "/app/chat", `(async () => { const i = document.querySelector("#chat-input"); i.value = "fake:reasoning"; i.dispatchEvent(new Event("input")); document.querySelector("#composer").requestSubmit(); for (let n = 0; n < 40 && !document.querySelector("#chat-log details"); n++) await new Promise((r) => setTimeout(r, 150)); await new Promise((r) => setTimeout(r, 500)); })()`], ["voices", "/app/voices", ""], ["admin", "/app/admin", ""],
];
const measure = `(() => {
  const round = (n) => Math.round(n);
  // A box of display: contents has no rectangles of its own, and is exactly what must not be missed.
  const found = [...document.querySelectorAll("details")].filter((d) => d.getClientRects().length > 0 || (getComputedStyle(d).display === "contents" && d.parentElement.getClientRects().length > 0));
  return found.map((d, index) => {
    const summary = d.querySelector(":scope > summary");
    d.open = true;
    const box = (d.getClientRects().length > 0 ? d : d.parentElement).getBoundingClientRect(), s = summary?.getBoundingClientRect();
    const kids = [...d.children].filter((c) => c !== summary && c.getClientRects().length > 0).map((c) => c.getBoundingClientRect());
    const c = kids.length ? { x: Math.min(...kids.map((k) => k.left)), y: Math.min(...kids.map((k) => k.top)), r: Math.max(...kids.map((k) => k.right)), b: Math.max(...kids.map((k) => k.bottom)) } : null;
    const parent = d.parentElement.getBoundingClientRect();
    const result = { index, cls: d.className || "(plain)", label: (summary?.textContent ?? "").trim().slice(0, 40), display: getComputedStyle(d).display,
      detailsW: round(box.width), parentW: round(parent.width), summaryLeft: s ? round(s.left - box.left) : null, summaryW: s ? round(s.width) : null,
      content: !!c, below: c && s ? round(c.y - s.bottom) : null, contentLeft: c ? round(c.x - box.left) : null, contentW: c ? round(c.r - c.x) : null, inside: c ? round(c.r) <= round(box.right) + 1 && round(c.x) >= round(box.left) - 1 : null, overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth };
    d.open = false;
    return result;
  });
})()`;
const rows: any[] = [];
for (const [width, height, mobile] of [[390, 844, true], [1280, 800, false]] as const) {
  await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: mobile ? 2 : 1, mobile });
  for (const [name, path, act] of PAGES) {
    await send("Page.navigate", { url: base + path }); await sleep(700);
    if (act) { await ev(act); await sleep(300); }
    const list = (await ev(measure)) ?? [];
    rows.push({ page: name, path, width, count: list.length, list });
  }
}
const supports = await ev(`CSS.supports("selector(::details-content)")`);
await Bun.write(out, JSON.stringify({ version, supports, rows }, null, 1));
console.log(version, "details-content:", supports, "details found:", rows.reduce((n, r) => n + r.count, 0));
ws.close(); process.exit(0);
