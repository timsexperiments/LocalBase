// Drives headless Chrome over the DevTools protocol for tools/axe-check.sh:
// opens each UI state, injects the pinned axe-core and prints violations.
// Exits 1 when any WCAG 2.x A/AA rule fails.
const [cdpBase, base, axePath] = process.argv.slice(2);
const axe = await Bun.file(axePath).text();
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

// [name, path, action run in the page before the scan]
const states: [string, string, string][] = [
  ["chat", "/app/chat", ""],
  ["picker", "/app/chat", `document.querySelector("[data-open-picker]").click()`],
  ["history", "/app/chat", `matchMedia("(min-width: 1024px)").matches || document.querySelector("[data-open-drawer]").click()`],
  ["generation", "/app/chat", `document.querySelector("[data-open-generation]").click()`],
  ["admin-people", "/app/admin?section=people", ""],
  ["admin-access", "/app/admin?section=access", ""],
  ["admin-keys", "/app/admin?section=keys", ""],
  ["lab-image", "/app/lab?tab=image", ""],
  ["lab-video", "/app/lab?tab=video", ""],
  ["lab-speech", "/app/lab?tab=speech", ""],
  ["lab-transcribe", "/app/lab?tab=transcribe", ""],
  ["settings", "/app/settings", ""],
  ["library", "/app/models", ""],
  ["get", "/app/models/get", ""],
  ["running", "/app/models/running", ""],
  ["voices", "/app/voices", ""],
];
const tags = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"];
await send("Page.enable");
await send("Runtime.enable");
let failures = 0;
for (const [width, mobile] of [[1280, false], [390, true]] as const) {
  await send("Emulation.setDeviceMetricsOverride", { width, height: mobile ? 844 : 900, deviceScaleFactor: mobile ? 3 : 1, mobile });
  await send("Emulation.setTouchEmulationEnabled", { enabled: mobile });
  for (const scheme of ["light", "dark"]) {
    await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
    for (const [name, path, action] of states) {
      await send("Page.navigate", { url: base + path });
      for (let i = 0; i < 50 && (await evaluate("document.readyState")) !== "complete"; i++) await sleep(100);
      await sleep(600);
      if (action) { await evaluate(action); await sleep(500); }
      await evaluate(axe);
      const res = await evaluate(`axe.run(document, { runOnly: { type: "tag", values: ${JSON.stringify(tags)} }, resultTypes: ["violations"] })
        .then((r) => ({ v: r.violations.map((v) => ({ id: v.id, impact: v.impact, tags: v.tags, help: v.help, nodes: v.nodes.map((n) => n.target.join(" ") + " :: " + (n.failureSummary || "").split("\\n").slice(1, 2).join("")) })), passes: 0 }))`);
      const label = `${String(width).padStart(4)} ${scheme.padEnd(5)} ${name}`;
      if (!res.v.length) { console.log(`ok   ${label}`); continue; }
      for (const v of res.v) {
        const wcag = v.tags.some((t: string) => /^wcag\d/.test(t));
        if (wcag) failures++;
        console.log(`${wcag ? "FAIL" : "note"} ${label}: ${v.id} (${v.impact}) ${v.help}`);
        for (const n of v.nodes.slice(0, 6)) console.log(`       ${n}`);
      }
    }
  }
}
ws.close();
console.log(failures ? `axe: ${failures} WCAG violation(s)` : "axe: no WCAG A/AA violations");
process.exit(failures ? 1 : 0);
