// Drives Chat's dictation in headless Chrome for tools/mic-check.sh: tap
// the microphone, let the fake device play the clip, tap again, and wait
// for the transcript in the message box.
const [cdpBase, base, expect] = process.argv.slice(2);
const target = await (await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let id = 0;
const waiting = new Map<number, (m: any) => void>();
ws.addEventListener("message", (e) => {
  const m = JSON.parse(String(e.data));
  if (m.id && waiting.has(m.id)) { waiting.get(m.id)!(m); waiting.delete(m.id); }
});
const send = (method: string, params: any = {}) => new Promise<any>((resolve, reject) => {
  const n = ++id;
  waiting.set(n, (m) => (m.error ? reject(new Error(`${method}: ${m.error.message}`)) : resolve(m.result)));
  ws.send(JSON.stringify({ id: n, method, params }));
});
const evaluate = async (expression: string) => {
  const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
  return r.result.value;
};
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
await send("Page.enable");
await send("Browser.grantPermissions", { origin: new URL(base).origin, permissions: ["audioCapture"] }).catch(() => {});
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
await send("Page.navigate", { url: `${base}/app/chat` });
for (let i = 0; i < 50 && (await evaluate("document.readyState")) !== "complete"; i++) await sleep(100);
await sleep(800);
const t0 = Date.now();
await evaluate(`document.querySelector("[data-dictate]").click()`);
await sleep(4000); // the clip plays (it loops; one pass is ~2.5 s)
await evaluate(`document.querySelector("[data-dictate]").click()`);
let text = "";
for (let i = 0; i < 120; i++) {
  text = await evaluate(`document.querySelector("#chat-input").value`);
  if (text.trim()) break;
  await sleep(250);
}
const status = await evaluate(`(document.querySelector("#chat-status")?.textContent || "") + " " + (document.querySelector("[data-error-box]")?.textContent || "")`);
console.log(`transcript (${((Date.now() - t0) / 1000).toFixed(1)} s from the first tap): ${JSON.stringify(text)}`);
if (status.trim()) console.log(`status: ${status.trim()}`);
ws.close();
const words = (s: string) => s.toLowerCase().replace(/[^a-z ]/g, " ").split(/\s+/).filter(Boolean);
const missing = words(expect).filter((w) => !words(text).includes(w));
console.log(missing.length ? `mic: missing words: ${missing.join(", ")}` : "mic: transcript matches");
process.exit(missing.length && !text.trim() ? 1 : 0);
