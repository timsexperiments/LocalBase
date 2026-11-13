// Two people, one browser profile, in a real browser, signed in through a fake OIDC provider over TLS.
// Run by test/browser/person-check.sh, which builds everything and passes: <cdp> <work dir> <first port>.
// Stops if the person isn't really signed in, and fails a row that nothing exercised, so a pass means
// something. Exits 1 when a check fails.
const [CDP, WORK, FIRST] = process.argv.slice(2);
const O = WORK, LAB = WORK, R = `${WORK}/root`;
const PORT = Number(FIRST), IDP_PORT = PORT, PROXY_PORT = PORT + 1, GATEWAY_PORT = PORT + 2, LLM_PORT = PORT + 3;
const IDP = `https://localhost:${IDP_PORT}`, ORIGIN = `https://localhost:${PROXY_PORT}`, GATEWAY = `http://127.0.0.1:${GATEWAY_PORT}`;
let fails = 0;
const check = (ok: boolean, msg: string) => { if (!ok) fails++; console.log(`${ok ? "ok  " : "FAIL"} ${msg}`); };
const note = (msg: string) => console.log(`NOTE ${msg}`);
const tls = { cert: Bun.file(`${O}/cert.pem`), key: Bun.file(`${O}/key.pem`) };
const pair = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
const jwk: any = { ...(await crypto.subtle.exportKey("jwk", pair.publicKey)), kid: "k1", use: "sig", alg: "RS256" };
const b64 = (b: ArrayBuffer | string) => Buffer.from(typeof b === "string" ? b : new Uint8Array(b)).toString("base64url");
let person: "alice" | "bob" = "alice";
let refuse = false; // the provider says no, so a page that loses its session stays signed out
const codes = new Map<string, any>();
Bun.serve({ port: IDP_PORT, tls, async fetch(req) {
  const u = new URL(req.url);
  if (u.pathname === "/.well-known/openid-configuration") return Response.json({ issuer: IDP, authorization_endpoint: `${IDP}/authorize`, token_endpoint: `${IDP}/token`, jwks_uri: `${IDP}/jwks`, response_types_supported: ["code"], subject_types_supported: ["public"], id_token_signing_alg_values_supported: ["RS256"], code_challenge_methods_supported: ["S256"], token_endpoint_auth_methods_supported: ["none"] });
  if (u.pathname === "/jwks") return Response.json({ keys: [jwk] });
  if (u.pathname === "/authorize") {
    const back = new URL(u.searchParams.get("redirect_uri")!); back.searchParams.set("state", u.searchParams.get("state")!);
    if (refuse) { back.searchParams.set("error", "access_denied"); return Response.redirect(back.href, 302); }
    const code = crypto.randomUUID(); codes.set(code, { nonce: u.searchParams.get("nonce"), client: u.searchParams.get("client_id"), person }); back.searchParams.set("code", code);
    return Response.redirect(back.href, 302);
  }
  if (u.pathname === "/token") {
    const form = new URLSearchParams(await req.text()); const c = codes.get(form.get("code") ?? ""); codes.delete(form.get("code") ?? "");
    if (!c) return Response.json({ error: "invalid_grant" }, { status: 400 });
    const now = Math.floor(Date.now() / 1000);
    const head = b64(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })), body = b64(JSON.stringify({ iss: IDP, aud: c.client, iat: now, exp: now + 600, nonce: c.nonce, email_verified: true, sub: c.person + "-sub", email: c.person + "@example.com" }));
    const sig = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${head}.${body}`));
    return Response.json({ access_token: "at", token_type: "Bearer", id_token: `${head}.${body}.${b64(sig)}` });
  }
  return new Response("not found", { status: 404 });
} });
Bun.serve({ port: PROXY_PORT, tls, async fetch(req) {
  const u = new URL(req.url), headers = new Headers(req.headers);
  const res = await fetch(`${GATEWAY}${u.pathname}${u.search}`, { method: req.method, headers, body: req.method === "GET" || req.method === "HEAD" ? undefined : await req.arrayBuffer(), redirect: "manual" });
  const out = new Headers(res.headers); out.delete("content-encoding"); out.delete("content-length");
  return new Response(res.status === 204 || res.status === 304 ? null : res.body, { status: res.status, headers: out });
} });
// The gateway reads the provider as it starts, so it starts now the provider is up.
// In its own process group, watched: it goes when this driver goes, even by kill -9.
const gateway = Bun.spawn(["perl", "-MPOSIX", "-e", "POSIX::setsid(); exec @ARGV", "sh", new URL("./supervise.sh", import.meta.url).pathname, String(process.pid), `${O}/local-base`, "--root", R, "--non-interactive", "serve", "--host", "127.0.0.1", "--port", String(GATEWAY_PORT), "--llm", "--llm-port", String(LLM_PORT), "--no-stt", "--no-tts", "--no-image", "--no-video"], { cwd: LAB, env: { ...process.env, LOCALBASE_LLAMA_SERVER: `${WORK}/llama-server`, LOCALBASE_COMPUTER_NAME: "Mac Studio" }, stdout: Bun.file(`${O}/serve.log`), stderr: Bun.file(`${O}/serve.log`), stdin: "ignore" });
// setsid then exec keeps the pid, so this is supervise.sh's, the leader of the gateway's group.
// kill-check.sh reads it to watch, and if need be end, that group and nothing else.
await Bun.write(`${O}/gateway.group`, String(gateway.pid));
for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"] as const) process.on(sig, () => process.exit(143));
for (let i = 0; i < 40; i++) { try { if ((await fetch(`${GATEWAY}/health`)).ok) break; } catch {} await new Promise((r) => setTimeout(r, 250)); }
const t = await (await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" })).json();
const ws = new WebSocket(t.webSocketDebuggerUrl); await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let id = 0; const wait = new Map();
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.id) { wait.get(m.id)?.(m.result ?? {}); wait.delete(m.id); } });
const send = (method: string, params: any = {}) => new Promise<any>((r) => { const i = ++id; wait.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
const ev = async (expression: string) => (await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true })).result?.value;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
await send("Page.enable"); await send("Network.enable");
await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
const go = async (path: string, ms = 1600) => { await send("Page.navigate", { url: ORIGIN + path }); await sleep(ms); };
const who = () => ev(`fetch("/app/session", { headers: { "x-localbase-ui": "1" } }).then((r) => r.ok ? r.json() : { status: r.status }).then((j) => j.email ?? j.name ?? j.principal?.name ?? JSON.stringify(j).slice(0, 80)).catch((e) => "error " + e)`);
const signIn = async (p: "alice" | "bob") => { person = p; await go("/app/login", 2200); const page = await ev("location.pathname"); if (page === "/app/login") { await ev(`document.querySelector("a[href*='/app/login'], main a.btn, main button")?.click()`); await sleep(2200); } const sess = await ev(`fetch("/app/session", { headers: { "x-localbase-ui": "1" } }).then((r) => r.text()).catch((e) => String(e))`); note(`signed in as ${p}: ${String(sess).slice(0, 120)}, at ${await ev("location.pathname + location.search")}`); if (!String(sess).includes(p + "@example.com")) { console.log(`STOP not signed in as ${p}, so nothing after this would mean anything. Page: ${String(await ev("document.title + \" :: \" + document.body.innerText.slice(0, 240)")).replace(/\s+/g, " ")}`); process.exit(3); } };
const signOut = async () => { await go("/app/settings", 1000); const did = await ev(`(() => { const b = document.querySelector("[data-logout]"); if (b) { b.click(); return true; } return false; })()`); await sleep(1500); note(`signed out (${did}): at ${await ev("location.pathname + location.search")}`); };
const optIn = async () => { await go("/app/settings", 1000); const was = await ev(`document.querySelector("[data-history-toggle]").checked`); if (!was) await ev(`document.querySelector("[data-history-toggle]").click()`); await sleep(200); return was; };
const say = async (text: string) => { await go("/app/chat", 1200); await ev(`(() => { const p = document.querySelector("#chat-input"); p.value = ${JSON.stringify(text)}; p.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector("[data-send]").click(); })()`); for (let i = 0; i < 40 && !(await ev(`[...document.querySelectorAll("[data-messages] article")].length >= 2 && !document.querySelector("[data-stop]:not([hidden])")`)); i++) await sleep(250); await sleep(400); };
const sees = (text: string) => ev(`(async () => { const page = document.body.textContent.includes(${JSON.stringify(text)}); document.querySelector("[data-open-drawer]")?.click(); await new Promise((r) => setTimeout(r, 150)); const list = [...document.querySelectorAll("[data-conversations]")].some((m) => m.textContent.includes(${JSON.stringify(text)})); return page || list; })()`);
const storage = () => ev(`(() => { const dump = (s) => Object.keys(s).filter((k) => k.startsWith("localbase")).map((k) => k + "=" + s.getItem(k).length); return { person: document.documentElement.dataset.person, local: dump(localStorage), session: dump(sessionStorage) }; })()`);
// Back, as the next person would press it. Says what's really on screen, and how the page came back.
// Read at the moment it comes back, after every pageshow handler, not a second and a half later.
// Paint isn't measured: script can't see a frame drawn before pageshow.
const returns: any[] = [], notUsed: string[] = [];
await send("Runtime.enable");
ws.addEventListener("message", (e) => { const m = JSON.parse(String(e.data)); if (m.method === "Runtime.consoleAPICalled" && m.params.args?.[0]?.value === "LB-BACK") returns.push(JSON.parse(m.params.args[1].value)); if (m.method === "Page.backForwardCacheNotUsed") notUsed.push(...(m.params.notRestoredExplanations ?? []).map((x: any) => x.reason)); });
await send("Page.addScriptToEvaluateOnNewDocument", { source: `addEventListener("pageshow", (e) => { window.__persisted = e.persisted; setTimeout(() => { const root = document.documentElement, body = document.body; const hid = (el) => !el || el.hidden || getComputedStyle(el).visibility === "hidden" || getComputedStyle(el).display === "none"; console.debug("LB-BACK", JSON.stringify({ persisted: e.persisted, where: location.pathname, words: (body?.textContent ?? "").includes("lake-from-alice"), hidden: hid(root) || hid(body) })); }, 0); });` });
const backTo = async (match: RegExp) => { const h = await send("Page.getNavigationHistory"); const here = h.currentIndex; for (let i = here - 1; i >= 0; i--) { if (match.test(h.entries[i].url)) { await send("Page.navigateToHistoryEntry", { entryId: h.entries[i].id }); await sleep(1500); return h.entries[i].url; } } return null; };
const showing = () => ev(`({ where: location.pathname + location.search, title: document.title, persisted: window.__persisted ?? null, nav: performance.getEntriesByType("navigation")[0]?.type, text: document.body.innerText.replace(/\\s+/g, " ").slice(0, 90) })`);
const A = "lake-from-alice", B = "barley-from-bob", T = "tab-only-from-alice", D = "draft-from-alice";

await signIn("alice");
const aliceKey = (await storage()).person;
check(!!aliceKey && !String(aliceKey).includes("alice"), `A's page has a person key that doesn't name them: ${aliceKey}`);
check((await optIn()) === false, "the setting starts off");
await say(A);
const onTitle = await ev("document.title");
check(String(onTitle).includes(A), `with saving on, the tab title has the chat's name: ${onTitle}`);
check(await sees(A), "A sees A's saved chat");
note(`A's storage: ${JSON.stringify(await storage())}`);
// A run still going: its place has the chat's name in it, so it has to be A's alone.
await ev(`(() => { const p = document.querySelector("#chat-input"); p.value = "fake:slow 400"; p.dispatchEvent(new Event("input", { bubbles: true })); document.querySelector("[data-send]").click(); })()`);
for (let i = 0; i < 20 && !(await ev(`Object.keys(localStorage).some((k) => k.startsWith("localbase.runs.places") && localStorage.getItem(k).length > 2)`)); i++) await sleep(150);
const places = await ev(`Object.keys(localStorage).filter((k) => k.startsWith("localbase.runs.places")).map((k) => [k, localStorage.getItem(k).slice(0, 160)])`);
note(`A's run places: ${JSON.stringify(places)}`);
check(places.length === 1 && places[0][0] === "localbase.runs.places.v2.${aliceKey}".replace("${aliceKey}", String(aliceKey)) && places[0][1].length > 2, "a running chat's place is under A's own key");
await ev(`document.querySelector("[data-stop]:not([hidden])")?.click()`);
await sleep(600);
await go(new URL(String(await ev("location.href"))).pathname + "?conversation=" + (await ev(`JSON.parse(localStorage.getItem("localbase.chat.history.v2.${aliceKey}")).find((c) => c.messages.some((m) => m.text.includes("${A}")))?.id`)), 1200);
const aliceChat = await ev("location.href");
const aliceTitle = onTitle;
note(`tab title for A's chat: ${aliceTitle}`);
await signOut();
// Back straight after signing out, before anyone signs in. The provider says no, as it would for
// someone with no account there, so a lost session stays lost.
refuse = true;
await sleep(300);
const justOut = await backTo(/\/app\/settings|\/app\/chat/);
const back0 = await showing();
note(`Back after sign-out went to ${justOut}: ${JSON.stringify(back0)}`);
check(!(await ev(`document.body.textContent.includes(${JSON.stringify(A)})`)), "Back after sign-out shows nothing of A's");
refuse = false;
await go("/app?signedout=1", 800);

await signIn("bob");
const bobKey = (await storage()).person;
check(!!bobKey && bobKey !== aliceKey, `B has a different key: ${bobKey}`);
await go("/app/chat");
check(!(await sees(A)), "B, on the same profile, sees nothing of A's in Chat");
const bobPlaces = await ev(`localStorage.getItem("localbase.runs.places.v2." + document.documentElement.dataset.person)`);
check(!String(bobPlaces ?? "").includes("alice"), `B's run places have nothing of A's: ${String(bobPlaces).slice(0, 60)}`);
// B presses Back to A's chat, in the same tab.
const reached = await backTo(/\/app\/chat\?conversation=/);
let steps = 0; const seen: any[] = [];
while (steps++ < 8) { const s = await showing(); seen.push(s); if (await ev(`document.body.textContent.includes(${JSON.stringify(A)})`)) { seen.push("A'S TEXT ON SCREEN"); break; } const more = await backTo(/\/app\/chat/); if (!more) break; }
note(`B pressing Back (first to ${reached}), ${seen.length} steps: ${JSON.stringify(seen).slice(0, 460)}`);
check(!seen.includes("A'S TEXT ON SCREEN"), "B pressing Back never brings A's chat on screen");
const restored = returns.filter((r) => r.persisted);
note(`pages restored from the cache: ${JSON.stringify(restored).slice(0, 260)}; not restored because: ${[...new Set(notUsed)].join(", ").slice(0, 160)}`);
check(restored.length > 0, restored.length ? `exercised: ${restored.length} pages really came back from the cache` : "NOT EXERCISED: nothing came back from the cache, so this proves nothing");
check(restored.every((r) => !r.words || r.hidden), `at the moment each came back, A's words were gone or the page blank: ${JSON.stringify(restored.filter((r) => r.words && !r.hidden)).slice(0, 160)}`);
check(restored.some((r) => r.words) , "and at least one of them really held A's words, so blanking mattered");
check(!seen.some((x: any) => x?.title === aliceTitle && aliceTitle !== "Chat · LocalBase"), "nor A's chat name in the tab title");
await go("/app/chat", 1000);
await go("/app/settings", 800);
check((await ev(`document.querySelector("[data-history-toggle]").checked`)) === false, "B's setting is off: A's choice isn't B's");
check(!(await ev(`document.body.textContent.includes(${JSON.stringify(A)})`)), "B's Settings shows nothing of A's");
await optIn();
await say(B);
check(await sees(B), "B sees B's own chat");
await signOut();

await signIn("alice");
await go("/app/chat");
check(await sees(A), "A back: A's chat is still there");
check(!(await sees(B)), "A doesn't see B's");
// A session that ends by itself keeps the chats. Sessions are removed as expiry removes them.
refuse = true;
const ended = Bun.spawn(["sqlite3", `${R}/local-base.db`, "delete from auth_browser_sessions"]); note(`sessions ended in the database: exit ${await ended.exited}`);
await go("/app/chat", 2200);
const afterEnd = String(await ev(`document.title + " :: " + location.pathname + location.search + " :: " + document.body.innerText.slice(0, 120)`)).replace(/\s+/g, " ");
note(`after the session ended: ${afterEnd}`);
check(!(await ev(`document.body.textContent.includes(${JSON.stringify(A)})`)), "the page after expiry shows nothing of A's");
check(!/\/app\/chat/.test(await ev("location.pathname")), "the session really ended: Chat isn't open");
check(await ev(`Object.keys(localStorage).some((k) => k === "localbase.chat.history.v2.${aliceKey}")`), "expiry didn't wipe A's saved chats");
refuse = false;
await signIn("alice");
await go("/app/chat");
check(await sees(A), "signed in again after expiry: A's chat is there");
// An admin takes away access, then gives it back.
const run = async (...args: string[]) => { const p = Bun.spawn([`${O}/local-base`, "--root", R, "--non-interactive", ...args], { stdout: "pipe", stderr: "pipe" }); const code = await p.exited; return `exit ${code} ${(await new Response(p.stdout).text()).replace(/\s+/g, " ").slice(0, 110)} ${(await new Response(p.stderr).text()).replace(/\s+/g, " ").slice(0, 110)}`; };
note(`users: ${await run("access", "users", "list")}`);
note(`disable alice: ${await run("access", "users", "disable", "--email=alice@example.com")}`);
await go("/app/chat", 2200);
const revoked = String(await ev(`document.title + " :: " + location.pathname + " :: " + document.body.innerText.slice(0, 160)`)).replace(/\s+/g, " ");
note(`after the revoke: ${revoked}`);
check(!/data-messages/.test(await ev(`document.querySelector("[data-messages]") ? "data-messages" : ""`)), "with access revoked, Chat isn't open");
check(!(await ev(`document.body.textContent.includes(${JSON.stringify(A)})`)), "with access revoked, the page shows nothing of A's");
check(await ev(`Object.keys(localStorage).some((k) => k === "localbase.chat.history.v2.${aliceKey}")`), "the revoke didn't wipe A's saved chats");
note(`enable alice: ${await run("access", "users", "enable", "--email=alice@example.com")}`);
await send("Network.clearBrowserCookies");
await signIn("alice");
await go("/app/chat");
check(await sees(A), "access back: A's chat is there");
// Without saving: this tab's chat and unsent words, across people in the same tab.
await go("/app/settings", 800);
await ev(`document.querySelector("[data-history-toggle]").click()`);
await sleep(200);
check(!(await ev(`Object.keys(localStorage).some((k) => k.startsWith("localbase.chat.history.v2.${aliceKey}"))`)), "turning saving off removes A's saved chats");
await say(T);
const offTitle = await ev("document.title");
check(offTitle === "Chat · LocalBase", `with saving off, the tab title is plain: ${offTitle}`);
await ev(`(() => { const p = document.querySelector("#chat-input"); p.value = ${JSON.stringify(D)}; p.dispatchEvent(new Event("input", { bubbles: true })); })()`);
await sleep(300);
note(`tab storage: ${JSON.stringify((await storage()).session)}`);
await signOut();
await signIn("bob");
await go("/app/chat");
check(!(await sees(T)), "B in the same tab doesn't get this tab's chat from A");
check((await ev(`document.querySelector("#chat-input").value`)) !== D, "nor A's unsent words");
// The tab title, directly, for each person in each state. saving(p, on) turns it on or off through the page.
const setSaving = async (on: boolean) => { await go("/app/settings", 900); if ((await ev(`document.querySelector("[data-history-toggle]").checked`)) !== on) await ev(`document.querySelector("[data-history-toggle]").click()`); await sleep(200); };
const titleAfter = async (text: string) => { await say(text); return String(await ev("document.title")); };
await signOut(); await signIn("alice"); await setSaving(false);
let tt = await titleAfter("title-off-alice"); check(tt === "Chat · LocalBase", `A saving off: ${tt}`);
await signOut(); await signIn("bob"); await setSaving(true);
tt = await titleAfter("title-on-bob"); check(tt.includes("title-on-bob"), `B saving on, with A off: ${tt}`);
await signOut(); await signIn("alice");
await go("/app/chat", 1000); check(!(String(await ev("document.title")).includes("title-on-bob")), "A doesn't get B's chat name");
await setSaving(true);
tt = await titleAfter("title-on-alice"); check(tt.includes("title-on-alice"), `A saving on: ${tt}`);
await signOut(); await signIn("bob"); await setSaving(false);
tt = await titleAfter("title-off-bob"); check(tt === "Chat · LocalBase", `B saving off, with A on: ${tt}`);
// Words typed and not sent, on a page loaded again as the next person: the browser's form restore,
// not the back/forward cache. Pushed out of that cache by opening other pages first.
await signOut(); await signIn("alice"); await go("/app/chat", 1000);
const typeInto = async (sel: string, text: string) => { await ev(`document.querySelector(${JSON.stringify(sel)}).focus()`); await send("Input.insertText", { text }); await sleep(120); };
await typeInto("#chat-input", "typed-not-sent-alice");
await ev(`document.querySelector("[data-open-drawer]").click()`); await sleep(150);
await typeInto("#chat-drawer [data-search]", "searched-by-alice");
const formPage = await ev("location.href");
await ev(`document.querySelector("#chat-drawer").close()`);
for (const p of ["/app/models", "/app/models/get", "/app/models/running", "/app/settings", "/app/studio", "/app/models?a=1", "/app/settings?a=2", "/app/models/running?a=3", "/app/studio?a=4"]) await go(p, 450);
await signOut(); await signIn("bob");
const hist = await send("Page.getNavigationHistory");
const entry = [...hist.entries].reverse().find((x: any) => x.url === formPage);
if (entry) { await send("Page.navigateToHistoryEntry", { entryId: entry.id }); await sleep(1600); }
const boxes = await ev(`({ where: location.pathname + location.search, nav: performance.getEntriesByType("navigation")[0]?.type, persisted: window.__persisted ?? null, composer: document.querySelector("#chat-input")?.value ?? null, search: [...document.querySelectorAll("[data-search]")].map((i) => i.value) })`);
note(`B back at the page where A typed: ${JSON.stringify(boxes)}`);
check(!!entry && boxes.persisted === false && /\/app\/chat/.test(boxes.where) && !!(boxes.nav === "back_forward" || boxes.nav === "reload"), entry ? `exercised: loaded again, not from the cache: ${boxes.nav}` : "NOT EXERCISED: A's page wasn't in the history");
check(boxes.composer !== null && !String(boxes.composer).includes("typed-not-sent-alice"), `B's message box doesn't hold A's unsent words: ${JSON.stringify(boxes.composer)}`);
check(boxes.search.length > 0 && !boxes.search.some((v: string) => v.includes("searched-by-alice")), `B's search doesn't hold A's: ${JSON.stringify(boxes.search)}`);
// The visible action.
await go("/app/settings", 800);
const summary = await ev(`document.querySelector("[data-history-settings] details.confirm summary")?.textContent.trim()`);
check(summary === "Clear saved chats on this device", `the action is there: ${summary}`);
const says = (await ev(`document.querySelector("[data-history-settings] .setting-row")?.textContent.replace(/\\s+/g, " ")`)) ?? "";
check(/stay on this device after you sign out/.test(says), `the setting says so: ${says.slice(0, 150)}`);
check(/can see their names in its history/.test(says) && /Turning this off removes them/.test(says), "what it costs and what turning it off does stay in view, with More closed");
const more = await ev(`(() => { const d = document.querySelector("[data-history-more]"); return { open: d?.open, says: d?.textContent.replace(/\\s+/g, " ") }; })()`);
check(more?.open === false && /aren’t encrypted/.test(more.says) && /never saved/.test(more.says), "the rest is behind More, closed at first");
await ev(`(() => { const d = document.querySelector("[data-history-settings] details.confirm"); d.open = true; d.querySelector("[data-history-clear]").click(); })()`);
await sleep(200);
check((await ev(`document.querySelector("[data-history-status]").textContent`)) === "Saved chats cleared from this device.", "Clear says it's done");
check(!(await ev(`Object.keys(localStorage).some((k) => k.startsWith("localbase.chat.history.v2.${bobKey}"))`)), "Clear removed B's saved chats");
await go("/app/chat");
check(!(await sees(B)), "B's chat is gone");
await signOut();
console.log(fails ? `person-check: ${fails} failed` : "person-check: all passed");
ws.close(); process.exit(fails ? 1 : 0);
