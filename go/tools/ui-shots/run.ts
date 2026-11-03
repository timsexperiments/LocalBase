// Drives headless Chrome for tools/ui-shots.sh: every page at every width in both color
// schemes, a contact sheet per scheme, and, with a baseline folder, a pixel comparison.
const [cdpBase, base, out, baseline] = process.argv.slice(2);
const WIDTHS: [number, number][] = [[320, 568], [360, 740], [390, 844], [430, 932], [768, 1024], [1280, 800]];
const SCHEMES = ["light", "dark"];
const only = process.env.UI_SHOTS_PAGES?.split(",");
const widthsOnly = process.env.UI_SHOTS_WIDTHS?.split(",").map(Number);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const wait = (ms: number) => `await new Promise((r)=>setTimeout(r,${ms}));`;
// [name, path, what to do first]. Actions leave the page in the state worth reviewing.
const PAGES: [string, string, string?][] = [
  ["chat", "/app/chat"],
  ["chat-reply", "/app/chat", `const p=document.querySelector("textarea");p.value="Plan a three day trip with kids. Keep it cheap.";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(2500)}`],
  ["chat-picker", "/app/chat", `document.querySelector("[data-open-picker]").click();${wait(450)}`],
  ["chat-long", "/app/chat", `const p=document.querySelector("textarea");p.value="Plan a three day trip with kids. Keep it cheap.";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(2500)}const r=document.querySelector(".reply");r.innerHTML="<p>Three days is enough if you keep the distances short and let the mornings do the work. Children tire by mid afternoon, so the plan puts the big thing first and leaves the rest of the day loose.</p><h3>Day one</h3><ul><li>Arrive early and walk the old town while it is still cool.</li><li>A long lunch somewhere with a courtyard.</li><li>The playground by the river, then an early dinner.</li></ul><h3>Day two</h3><p>Take the morning train out to the lake. Tickets are cheaper before nine, and children under six ride free on most lines, which you should check before you buy.</p><pre><code>08:10 train\\n09:05 lake\\n13:30 back</code></pre><p>Pack lunch the night before. It saves more than any other choice on this list, and nobody has to decide anything when everyone is hungry.</p>";window.scrollTo(0,0);document.querySelector("#chat-log")?.scrollTo(0,99999);${wait(200)}`],
  ["chat-failed", "/app/chat", `window.fetch=async()=>new Response(JSON.stringify({error:{code:"model_too_large",param:"model",message:"This model requires about 14.5 GiB of memory, but only 4.7 GiB is available for models on this host (4.7 GiB total, after the safety reserve). It cannot be loaded here even when nothing else is running. Use a smaller quantization or a smaller model."}}),{status:422,headers:{"content-type":"application/json"}});const p=document.querySelector("textarea");p.value="Hello there.";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(1500)}`],
  ["strip-long", "/app/studio", `const b=document.querySelector(".strip-host b");b.textContent="Workshop Mac Studio in Attic";b.title=b.textContent;document.querySelector(".strip-word").textContent="Getting the model ready";`],
  ["strip-cjk", "/app/studio", `const b=document.querySelector(".strip-host b");b.textContent="工房のマックスタジオ工房のマックスタジオ";b.title=b.textContent;document.querySelector(".strip-word").textContent="Getting the model ready";`],
  ["strip-unknown", "/app/studio", `document.querySelector(".strip-word").textContent="Ready";document.querySelector(".strip-free").textContent="Memory reading unavailable";document.querySelector(".gauge").innerHTML=Array.from({length:48},(_,i)=>'<rect x="'+i*10+'" y="9" width="6" height="2" class="gauge-unknown"/>').join("");`],
  ["chat-preparing", "/app/chat", `const f=window.fetch;window.fetch=(u,o)=>o&&o.method==="POST"?new Promise(()=>{}):f(u,o);document.querySelector("[data-strip-word]").textContent="Getting the model ready";const p=document.querySelector("textarea");p.value="Plan a three-day trip on a small budget.";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(500)}`],
  ["chat-streaming", "/app/chat", `const p=document.querySelector("textarea");p.value="fake:slow 80";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(2000)}`],
  ["chat-stopped", "/app/chat", `const p=document.querySelector("textarea");p.value="fake:slow 80";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(1200)}document.querySelector("[data-stop]").click();${wait(700)}`],
  ["chat-interrupted", "/app/chat", `const p=document.querySelector("textarea");p.value="fake:truncated";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(2500)}`],
  ["chat-keyboard", "/app/chat", `document.querySelector("#chat-input").focus();${wait(100)}document.documentElement.setAttribute("data-typing","");${wait(150)}`],
  ["first-reply", "/app/chat?ready=1", `const p=document.querySelector("textarea");p.value="fake:reply Three days is plenty if you keep the distances short.\\n\\n**Day one.** Walk the old town early, while it is cool, and have a long lunch somewhere with a courtyard.\\n\\n**Day two.** Take the morning train out to the lake. Tickets are usually cheaper before nine, so it is worth checking the times the night before.\\n\\n**Day three.** Keep it loose. Pack lunch, pick one thing you missed, and leave time for the trip home.";p.dispatchEvent(new Event("input",{bubbles:true}));${wait(100)}document.querySelector("button[type=submit]").click();${wait(2000)}const u=document.querySelector("[data-messages] article");const words="Plan a three-day trip on a small budget.";for(const n of u.querySelectorAll("*")){if(n.children.length===0&&n.textContent.startsWith("fake:reply"))n.textContent=words;}document.getElementById("chat-title").textContent="Plan a three-day trip on";document.querySelector("textarea").blur();${wait(100)}`],
  ["welcome", "/app/welcome"],
  ["welcome-choose", "/app/welcome/choose"],
  ["studio", "/app/studio"],
  ["image", "/app/studio/image"],
  ["image-viewer", "/app/studio/image", `document.querySelector(".image-open")?.click();${wait(400)}`],
  ["video", "/app/studio/video"],
  ["speech", "/app/studio/speech"],
  ["transcribe", "/app/studio/transcribe"],
  ["decisions", "/app/studio/decisions"],
  ["decisions-result", "/app/studio/decisions", `document.querySelector("[data-open-sheet=decide-template]").click();${wait(120)}document.querySelector('[data-template="route"]').click();${wait(120)}document.querySelector("#decide .lab-dock button[type=submit]").click();${wait(1800)}`],
  ["models", "/app/models"],
  ["get", "/app/models/get"],
  ["running", "/app/models/running"],
  ["voices", "/app/voices"],
  ["settings", "/app/settings"],
  ["admin", "/app/admin?section=keys"],
  ["more", "/app/models", `[...document.querySelectorAll("nav a, nav button, button")].find((b)=>/More/.test(b.textContent))?.click();${wait(400)}`],
  ["not-found", "/app/nope"],
];
// Nothing moves. When comparing with a baseline, live figures are hidden too, as they change
// by themselves; contact sheets keep them, because they are the point.
const STEADY = `const s=document.createElement("style");s.textContent="*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}";document.head.append(s);${baseline ? `for(const n of document.querySelectorAll("[data-strip-free],.gauge,.gauge-legend dd,#running-live .text-muted"))n.style.visibility="hidden";` : ""}`;

const target = await (await fetch(`${cdpBase}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let id = 0; const waiting = new Map();
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.id && waiting.has(m.id)) { waiting.get(m.id)(m); waiting.delete(m.id); } });
const send = (method: string, params: any = {}) => new Promise<any>((res, rej) => { const n = ++id; waiting.set(n, (m: any) => (m.error ? rej(new Error(method + ": " + m.error.message)) : res(m.result))); ws.send(JSON.stringify({ id: n, method, params })); });
const ev = async (expression: string) => { const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }); if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300)); return r.result.value; };
await send("Page.enable");

const shots: { page: string; width: number; scheme: string; file: string }[] = [];
// UI_SHOTS_SHEETS_ONLY=1 redraws the sheets from the captures already in the folder.
const sheetsOnly = !!process.env.UI_SHOTS_SHEETS_ONLY;
if (sheetsOnly) for (const [name] of PAGES) for (const [width] of WIDTHS) for (const scheme of SCHEMES) {
  const file = `${name}-${width}-${scheme}.png`;
  if (await Bun.file(`${out}/${file}`).exists()) shots.push({ page: name, width, scheme, file });
}
for (const [name, path, action] of sheetsOnly ? [] : PAGES) {
  if (only && !only.includes(name)) continue;
  for (const [width, height] of WIDTHS.filter(([w]) => !widthsOnly || widthsOnly.includes(w))) {
    for (const scheme of SCHEMES) {
      const mobile = width < 1024;
      await send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: mobile ? 2 : 1, mobile });
      await send("Emulation.setTouchEmulationEnabled", { enabled: mobile });
      await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }, { name: "prefers-reduced-motion", value: "reduce" }] });
      await send("Page.navigate", { url: base + path });
      await sleep(900);
      if (action) await ev(`(async()=>{${action}})()`);
      await ev(`(()=>{${STEADY}})()`);
      await ev(`document.fonts.ready.then(()=>true)`);
      await sleep(120);
      const overflow = await ev(`document.documentElement.scrollWidth-innerWidth`);
      if (overflow > 0) console.log(`OVERFLOW ${name} ${width} ${scheme}: ${overflow}px`);
      const { data } = await send("Page.captureScreenshot", { format: "png" });
      const file = `${name}-${width}-${scheme}.png`;
      await Bun.write(`${out}/${file}`, Buffer.from(data, "base64"));
      shots.push({ page: name, width, scheme, file });
    }
  }
  console.log(name);
}

// A sheet per scheme: a row per page, a column per width.
await send("Emulation.clearDeviceMetricsOverride");
await send("Emulation.setDeviceMetricsOverride", { width: 2600, height: 1200, deviceScaleFactor: 1, mobile: false });
// Parts, because a browser won't paint a page taller than about 16000 px: that came out blank.
const SHEET_ROWS = 10;
for (const scheme of SCHEMES) {
  const pages = [...new Set(shots.map((s) => s.page))];
  for (let part = 0; part * SHEET_ROWS < pages.length; part++) {
  const rows = await Promise.all(pages.slice(part * SHEET_ROWS, (part + 1) * SHEET_ROWS).map(async (p) => {
    const cells = await Promise.all(shots.filter((s) => s.page === p && s.scheme === scheme).map(async (s) => `<figure><figcaption>${p} ${s.width}</figcaption><img src="${s.file}"></figure>`));
    return `<div class="r">${cells.join("")}</div>`;
  }));
  const html = `<!doctype html><meta charset="utf-8"><style>body{margin:8px;background:#555;color:#fff;font:600 12px system-ui}.r{display:flex;gap:8px;margin-bottom:10px;align-items:flex-start}figure{margin:0}img{height:440px;display:block}</style>${rows.join("")}`;
  const name = `_sheet-${scheme}-${part + 1}`;
  await Bun.write(`${out}/${name}.html`, html);
  await send("Emulation.setDeviceMetricsOverride", { width: 2600, height: 1200, deviceScaleFactor: 1, mobile: false });
  await send("Page.navigate", { url: Bun.pathToFileURL(`${out}/${name}.html`).href });
  for (let i = 0; i < 100 && !(await ev(`[...document.images].every((i) => i.complete)`)); i++) await sleep(100);
  const broken = await ev(`document.images.length ? [...document.images].filter((i) => !i.naturalWidth).length : -1`);
  if (broken) throw new Error(`${name}: ${broken} pictures did not load`);
  const h = await ev(`document.documentElement.scrollHeight`);
  await send("Emulation.setDeviceMetricsOverride", { width: 2600, height: h, deviceScaleFactor: 1, mobile: false });
  await sleep(600);
  const { data } = await send("Page.captureScreenshot", { format: "png" });
  const png = Buffer.from(data, "base64");
  // A one-colour page compresses to next to nothing. Never hand one to a reviewer.
  if (png.length < 150_000) throw new Error(`${name}.png came out blank (${png.length} bytes)`);
  await Bun.write(`${out}/${name}.png`, png);
  await Bun.$`rm -f ${out}/${name}.html`;
  }
}

// With a baseline: how much of each picture changed.
let changed = 0;
if (baseline) {
  await send("Page.navigate", { url: "about:blank" });
  for (const s of shots) {
    const old = Bun.file(`${baseline}/${s.file}`);
    if (!(await old.exists())) { console.log(`NEW ${s.file}`); continue; }
    const a = Buffer.from(await Bun.file(`${out}/${s.file}`).arrayBuffer()).toString("base64");
    const b = Buffer.from(await old.arrayBuffer()).toString("base64");
    const pct = await ev(`(async()=>{const load=(d)=>new Promise((r,x)=>{const i=new Image();i.onload=()=>r(i);i.onerror=x;i.src="data:image/png;base64,"+d});const [x,y]=await Promise.all([load(${JSON.stringify(a)}),load(${JSON.stringify(b)})]);if(x.width!==y.width||x.height!==y.height)return 100;const c=(i)=>{const k=document.createElement("canvas");k.width=i.width;k.height=i.height;const g=k.getContext("2d");g.drawImage(i,0,0);return g.getImageData(0,0,i.width,i.height).data};const p=c(x),q=c(y);let n=0;for(let i=0;i<p.length;i+=4)if(Math.abs(p[i]-q[i])+Math.abs(p[i+1]-q[i+1])+Math.abs(p[i+2]-q[i+2])>30)n++;return n/(p.length/4)*100})()`);
    if (pct > 0.5) { changed++; console.log(`CHANGED ${s.file}: ${pct.toFixed(1)}%`); }
  }
  console.log(changed ? `ui-shots: ${changed} picture(s) changed` : "ui-shots: no picture changed");
}
ws.close();
process.exit(0);
