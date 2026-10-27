// Drives headless Chrome over the DevTools protocol for tools/phone-check.sh.
// Exits 1 when any check fails.
const [cdpBase, base] = process.argv.slice(2);
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
const viewport = (width: number, height: number, mobile: boolean) => Promise.all([
  send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: mobile ? 3 : 1, mobile }),
  send("Emulation.setTouchEmulationEnabled", { enabled: mobile }),
  send("Emulation.setEmitTouchEventsForMouse", { enabled: mobile }),
]);
const open = async (path: string) => {
  await send("Page.navigate", { url: base + path });
  for (let i = 0; i < 50 && (await evaluate("document.readyState")) !== "complete"; i++) await sleep(100);
  await sleep(400);
};

const pages = ["/app/chat", "/app/models", "/app/models/get", "/app/models/running", "/app/lab?tab=image", "/app/lab?tab=video",
  "/app/lab?tab=speech", "/app/lab?tab=transcribe", "/app/voices", "/app/settings", "/app/admin?section=keys", "/app/admin?section=access"];

// Controls under 44x44 CSS px. Inline links in running text are exempt
// (WCAG 2.5.8's inline exception), as are hidden and disabled-looking ones.
const smallTargets = `(() => {
  const out = [];
  for (const el of document.querySelectorAll("a[href], button, input, select, textarea, summary")) {
    if (el.type === "hidden" || el.closest("[hidden], dialog:not([open]), [popover]:not(:popover-open)")) continue;
    const cs = getComputedStyle(el);
    if (cs.display === "none" || cs.visibility === "hidden") continue;
    const r = el.getBoundingClientRect();
    if (r.width <= 1 || r.height <= 1) continue; // sr-only until focused
    if (el.tagName === "A" && cs.display === "inline") continue;
    if (el.type === "checkbox" || el.type === "radio") {
      const label = el.closest("label");
      const lr = label?.getBoundingClientRect();
      if (lr && lr.height >= 44) continue;
    }
    if (r.height < 43.5 || r.width < 43.5) out.push(el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + " " + JSON.stringify((el.textContent || el.getAttribute("aria-label") || el.name || "").trim().slice(0, 30)) + " " + Math.round(r.width) + "x" + Math.round(r.height));
  }
  return out;
})()`;
const overflow = `(() => {
  const w = document.documentElement.clientWidth;
  const wide = [...document.querySelectorAll("body *")].filter((el) => {
    const r = el.getBoundingClientRect();
    return r.width && r.right > w + 1 && getComputedStyle(el).position !== "fixed" && !el.closest("dialog:not([open]), [hidden], .overflow-x-auto, .overflow-hidden, .overflow-y-auto, pre");
  }).slice(0, 4).map((el) => el.tagName.toLowerCase() + "." + [...el.classList].slice(0, 3).join("."));
  return { scroll: document.scrollingElement.scrollWidth - w, wide };
})()`;
const visible = (sel: string) => `(() => { const el = document.querySelector(${JSON.stringify(sel)}); if (!el) return false; const r = el.getBoundingClientRect(); return getComputedStyle(el).display !== "none" && r.width > 0 && r.height > 0; })()`;

let failures = 0;
const fail = (label: string, msg: string) => { failures++; console.log(`FAIL ${label}: ${msg}`); };

await send("Page.enable");
await send("Runtime.enable");
for (const [width, height, mobile] of [[360, 740, true], [390, 844, true], [430, 932, true], [1280, 900, false]] as const) {
  await viewport(width, height, mobile);
  for (const path of pages) {
    const label = `${String(width).padStart(4)} ${path}`;
    await open(path);
    const before = failures;
    const o = await evaluate(overflow);
    if (o.scroll > 0) fail(label, `scrolls sideways by ${o.scroll}px (${o.wide.join(", ")})`);
    const small = await evaluate(smallTargets);
    for (const s of small.slice(0, 6)) fail(label, `target under 44px: ${s}`);
    const tabbar = await evaluate(visible("nav.tabbar"));
    const sidebar = await evaluate(visible("aside.sidebar"));
    if (mobile && (!tabbar || sidebar)) fail(label, `phone shell: tab bar ${tabbar}, sidebar ${sidebar}`);
    if (!mobile && (tabbar || !sidebar)) fail(label, `desktop shell: tab bar ${tabbar}, sidebar ${sidebar}`);
    if (mobile) {
      // Nothing scrollable hides under the fixed tab bar: the last element's bottom clears it.
      const covered = await evaluate(`(() => {
        if (document.body.classList.contains("overflow-hidden")) return 0;
        const bar = document.querySelector("nav.tabbar").getBoundingClientRect();
        window.scrollTo(0, document.scrollingElement.scrollHeight);
        const main = document.querySelector("main");
        const last = [...main.querySelectorAll("*")].filter((el) => el.getBoundingClientRect().height).pop();
        const gap = last ? bar.top - last.getBoundingClientRect().bottom : 0;
        window.scrollTo(0, 0);
        return gap < 0 ? -gap : 0;
      })()`);
      if (covered) fail(label, `the tab bar covers the end of the page by ${Math.round(covered)}px`);
    }
    if (failures === before) console.log(`ok   ${label}`);
  }
}

// The More sheet: opens as a modal from the tab bar, focus moves into it,
// Escape closes it and focus returns to the More button.
await viewport(390, 844, true);
await open("/app/models");
await evaluate(`document.querySelector("[data-open-sheet=more-sheet]").focus(); document.querySelector("[data-open-sheet=more-sheet]").click()`);
await sleep(300);
const sheet = await evaluate(`({ open: document.getElementById("more-sheet").open, inside: document.getElementById("more-sheet").contains(document.activeElement), links: [...document.querySelectorAll("#more-sheet a")].map((a) => a.textContent.trim()) })`);
if (!sheet.open || !sheet.inside) fail("more sheet", JSON.stringify(sheet));
await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
await sleep(300);
const after = await evaluate(`({ open: document.getElementById("more-sheet").open, back: document.activeElement?.dataset?.openSheet })`);
if (after.open || after.back !== "more-sheet") fail("more sheet", `after Escape ${JSON.stringify(after)}`);
else console.log(`ok   more sheet (${sheet.links.join(", ")})`);

// The keyboard: focusing a text field on a touch screen hides the tab bar;
// when the viewport shrinks (as Chrome on Android resizes for the keyboard)
// the focused field is scrolled into view.
for (const [path, field] of [["/app/lab?tab=image", "textarea[name=prompt]"], ["/app/voices", "input[name=name]"], ["/app/chat", "#chat-input"]]) {
  await viewport(390, 844, true);
  await open(path);
  const exists = await evaluate(`!!document.querySelector(${JSON.stringify(field)}) && !document.querySelector(${JSON.stringify(field)}).disabled`);
  if (!exists) { console.log(`skip keyboard ${path} (no enabled ${field} on this gateway)`); continue; }
  await evaluate(`document.querySelector(${JSON.stringify(field)}).focus()`);
  await viewport(390, 844 - 336, true);
  await sleep(700);
  const k = await evaluate(`(() => {
    const el = document.querySelector(${JSON.stringify(field)}); const r = el.getBoundingClientRect();
    return { typing: document.documentElement.hasAttribute("data-typing"), tabbar: ${visible("nav.tabbar")}, top: r.top, bottom: r.bottom, h: innerHeight };
  })()`);
  const label = `keyboard ${path}`;
  if (!k.typing || k.tabbar) fail(label, `typing ${k.typing}, tab bar visible ${k.tabbar}`);
  else if (k.top < 0 || k.top > k.h - 44) fail(label, `field out of view (top ${Math.round(k.top)}, viewport ${k.h})`);
  else console.log(`ok   ${label}`);
}

ws.close();
console.log(failures ? `phone-check: ${failures} failure(s)` : "phone-check: all checks passed");
process.exit(failures ? 1 : 0);
