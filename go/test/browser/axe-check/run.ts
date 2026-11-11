// Drives headless Chrome over the DevTools protocol for tools/axe-check.sh:
// opens each UI state, injects the pinned axe-core and prints violations.
// Exits 1 when any WCAG 2.x A/AA rule fails.
import { evaluate as evaluateIn, openPage } from "../cdp/cdp.ts";

const [cdpBase, base, axePath] = process.argv.slice(2);
const axe = await Bun.file(axePath).text();
// With its browser gone nothing here can finish: the client stops the run.
const { send, ws } = await openPage(cdpBase);
const evaluate = (expression: string) => evaluateIn(send, expression);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// [name, path, action run in the page before the scan]
const states: [string, string, string][] = [
  ["chat", "/app/chat", ""],
  ["welcome", "/app/welcome"],
  ["welcome-choose", "/app/welcome/choose"],
  ["not-found", "/app/nope"],
  ["sign-in-link", "/app/login/once"],
  ["picker", "/app/chat", `document.querySelector("[data-open-picker]").click()`],
  ["history", "/app/chat", `matchMedia("(min-width: 1024px)").matches || document.querySelector("[data-open-drawer]").click()`],
  ["generation", "/app/chat", `document.querySelector("[data-open-generation]").click()`],
  ["admin-people", "/app/admin?section=people", ""],
  ["admin-access", "/app/admin?section=access", ""],
  ["admin-keys", "/app/admin?section=keys", ""],
  ["studio", "/app/studio", ""],
  ["lab-image", "/app/studio/image", ""],
  ["image-shape", "/app/studio/image", `document.querySelector("[data-open-sheet=image-shape]")?.click()`],
  ["image-viewer", "/app/studio/image", `document.querySelector(".image-open")?.click()`],
  ["lab-video", "/app/studio/video", ""],
  ["video-sheet", "/app/studio/video", `document.querySelector("[data-video-chip]")?.click()`],
  ["lab-speech", "/app/studio/speech", ""],
  ["speech-voices", "/app/studio/speech", `document.querySelector("[data-open-sheet=lab-voice]")?.click()`],
  ["lab-transcribe", "/app/studio/transcribe", ""],
  ["transcribe-language", "/app/studio/transcribe", `document.querySelector("[data-open-sheet=lab-language]")?.click()`],
  ["decisions", "/app/studio/decisions", ""],
  ["decisions-answer", "/app/studio/decisions", `(async () => { document.querySelector("[data-open-sheet=decide-template]").click(); await new Promise((r) => setTimeout(r, 50)); document.querySelector("[data-template=route]").click(); document.querySelector("#decide button[type=submit]").click(); await new Promise((r) => setTimeout(r, 1500)); })()`],
  ["decisions-code", "/app/studio/decisions", `document.querySelector("[data-code]").click()`],
  ["settings", "/app/settings", ""],
  ["library", "/app/models", ""],
  ["get", "/app/models/get", ""],
  ["running", "/app/models/running", ""],
  ["voices", "/app/voices", ""],
];
const tags = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"];
await send("Page.enable");
await send("Runtime.enable");
await send("Network.enable");
// A gateway with sign-in on: AXE_COOKIE="name=value" scans as that session. Get one from
// `local-base access login-link` on a scratch gateway.
if (process.env.AXE_COOKIE) {
  const at = process.env.AXE_COOKIE.indexOf("=");
  await send("Network.setCookie", { name: process.env.AXE_COOKIE.slice(0, at), value: process.env.AXE_COOKIE.slice(at + 1), url: base });
}
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
