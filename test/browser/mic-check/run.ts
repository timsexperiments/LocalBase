// Drives Chat's dictation in headless Chrome for test/browser/mic-check.sh: tap
// the microphone, let the fake device play the clip, tap again, and wait
// for the transcript in the message box.
import { evaluate as evaluateIn, openPage } from "../cdp/cdp.ts";

const [cdpBase, base, expect] = process.argv.slice(2);
// With its browser gone nothing here can finish: the client stops the run.
const { send, ws } = await openPage(cdpBase);
const evaluate = (expression: string) => evaluateIn(send, expression);
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
