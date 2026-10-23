// Decision-model qualification (opt-in, operator-run; not in CI: it needs a
// real decision model): 45 checks of one decision model on a running
// gateway. Discovery, response structure, a labelled choice/noul/score set,
// several questions in one call, determinism, latency and concurrency,
// image input, the context limit, validation and permissions.
//
//   LOCALBASE_QUALIFY_URL=http://127.0.0.1:<port> \
//   LOCALBASE_QUALIFY_MODEL=clef-flash-q4_k_m \
//   LOCALBASE_QUALIFY_KEY=<key> LOCALBASE_QUALIFY_NOSCOPE_KEY=<key> \
//     bun run go/tools/decision-qualify.ts
//
// LOCALBASE_QUALIFY_KEY needs models:read, inference:decision and
// inference:chat (the chat and embeddings refusal checks pass the
// permission check first). LOCALBASE_QUALIFY_NOSCOPE_KEY is a key without
// inference:decision; without it that check is skipped. Keys are read from
// the environment only and never printed. The first request loads the model
// (a real GPU load): run it in an operator window. Exits 1 if a check fails.
import { deflateSync } from "node:zlib";

const need = (name: string) => {
  const v = process.env[name]?.trim();
  if (!v) {
    console.error(`decision-qualify: set ${name}`);
    process.exit(2);
  }
  return v;
};
const BASE = need("LOCALBASE_QUALIFY_URL").replace(/\/+$/, "");
const MODEL = need("LOCALBASE_QUALIFY_MODEL");
const okKey = need("LOCALBASE_QUALIFY_KEY");
const noScopeKey = process.env.LOCALBASE_QUALIFY_NOSCOPE_KEY?.trim() || null;

type Row = { check: string; pass: boolean; detail: string };
const rows: Row[] = [];
const rec = (check: string, pass: boolean, detail = "") => {
  rows.push({ check, pass, detail });
  console.log(`${pass ? "PASS" : "FAIL"}  ${check}${detail ? "  — " + detail : ""}`);
};

async function post(path: string, body: unknown, key: string | null = okKey) {
  const t0 = performance.now();
  const res = await fetch(BASE + path, {
    method: "POST",
    headers: { "content-type": "application/json", ...(key ? { authorization: `Bearer ${key}` } : {}) },
    body: typeof body === "string" ? body : JSON.stringify(body),
    signal: AbortSignal.timeout(180_000),
  });
  const text = await res.text();
  let json: any = null;
  try { json = JSON.parse(text); } catch {}
  return { status: res.status, json, ms: performance.now() - t0, text };
}
const decide = (state: unknown, questions: unknown, extra: Record<string, unknown> = {}) =>
  post("/v1/systemone", { model: MODEL, state, questions, ...extra });

const sum = (o: Record<string, number>) => Object.values(o).reduce((a, b) => a + b, 0);
const argmax = (o: Record<string, number>) => Object.entries(o).sort((a, b) => b[1] - a[1])[0][0];
const median = (xs: number[]) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];

// ---- 1. Discovery -------------------------------------------------------------
{
  const list = await fetch(`${BASE}/v1/models`, { headers: { authorization: `Bearer ${okKey}` } }).then((r) => r.json() as any);
  const m = list.data?.find((x: any) => x.id === MODEL);
  rec("model is listed in /v1/models", !!m);
  const one = await fetch(`${BASE}/v1/models/${MODEL}`, { headers: { authorization: `Bearer ${okKey}` } });
  rec("GET /v1/models/{id} returns the model", one.status === 200, `status ${one.status}`);
}

// ---- 2. First request (includes switching to and loading the decision model) -----
const invoiceQ = {
  status: { type: "choice", instructions: "What is the invoice status?", criteria: { overdue: "Past the due date and unpaid", paid: "Already paid", open: "Not yet due and unpaid" } },
};
const overdueState = { invoice: "INV-104", due: "2026-09-01", today: "2026-10-01", paid: false };
const first = await decide(overdueState, invoiceQ);
const coldMs = first.ms;
rec("first request succeeds (cold: switch + load)", first.status === 200, `${first.status}, ${(coldMs / 1000).toFixed(1)} s`);

// ---- 3. Response structure ------------------------------------------------------
{
  const a = first.json?.answers?.status;
  rec("choice: has choice, probabilities, confidence", !!a && typeof a.choice === "string" && !!a.probabilities && typeof a.confidence === "number");
  if (a?.probabilities) {
    rec("choice: probability keys equal the options", JSON.stringify(Object.keys(a.probabilities).sort()) === JSON.stringify(["open", "overdue", "paid"]));
    rec("choice: probabilities sum to 1", Math.abs(sum(a.probabilities) - 1) < 1e-3, `sum ${sum(a.probabilities).toFixed(6)}`);
    rec("choice: chosen option is the most probable", argmax(a.probabilities) === a.choice, `${a.choice}`);
  }
  rec("usage reports input and output tokens", typeof first.json?.usage?.input_tokens === "number" && typeof first.json?.usage?.output_tokens === "number", JSON.stringify(first.json?.usage));
  rec("response names the model", first.json?.model === MODEL);
}

// ---- 4. Labeled sanity set ---------------------------------------------------------
const choiceCases: { name: string; state: unknown; instructions: string; criteria: Record<string, string | null>; expect: string }[] = [
  { name: "invoice overdue", state: overdueState, instructions: "What is the invoice status?", criteria: { overdue: "Past the due date and unpaid", paid: "Already paid", open: "Not yet due and unpaid" }, expect: "overdue" },
  { name: "invoice open", state: { invoice: "INV-9", due: "2026-12-01", today: "2026-10-01", paid: false }, instructions: "What is the invoice status?", criteria: { overdue: "Past the due date and unpaid", paid: "Already paid", open: "Not yet due and unpaid" }, expect: "open" },
  { name: "invoice paid", state: { invoice: "INV-7", due: "2026-09-01", today: "2026-10-01", paid: true }, instructions: "What is the invoice status?", criteria: { overdue: "Past the due date and unpaid", paid: "Already paid", open: "Not yet due and unpaid" }, expect: "paid" },
  { name: "ticket billing", state: "Customer message: I was charged twice for the same order.", instructions: "Which team should handle this ticket?", criteria: { billing: "Charges, refunds, invoices", shipping: "Delivery and tracking", technical: "Bugs and crashes" }, expect: "billing" },
  { name: "ticket shipping", state: "Customer message: My package never arrived and tracking has been stuck for two weeks.", instructions: "Which team should handle this ticket?", criteria: { billing: "Charges, refunds, invoices", shipping: "Delivery and tracking", technical: "Bugs and crashes" }, expect: "shipping" },
  { name: "ticket technical", state: "Customer message: The app crashes every time I open the settings page.", instructions: "Which team should handle this ticket?", criteria: { billing: "Charges, refunds, invoices", shipping: "Delivery and tracking", technical: "Bugs and crashes" }, expect: "technical" },
  { name: "sentiment positive", state: "Review: I love this, it works perfectly and setup took two minutes!", instructions: "What is the sentiment?", criteria: { positive: null, negative: null, neutral: null }, expect: "positive" },
  { name: "sentiment negative", state: "Review: Terrible. It broke after one day and support ignored me.", instructions: "What is the sentiment?", criteria: { positive: null, negative: null, neutral: null }, expect: "negative" },
];
let correct = 0;
for (const c of choiceCases) {
  const r = await decide(c.state, { q: { type: "choice", instructions: c.instructions, criteria: c.criteria } });
  const got = r.json?.answers?.q?.choice;
  const conf = r.json?.answers?.q?.confidence;
  const ok = r.status === 200 && got === c.expect;
  if (ok) correct++;
  rec(`choice sanity: ${c.name}`, ok, `got ${got} (confidence ${typeof conf === "number" ? conf.toFixed(2) : "?"}), ${r.status}`);
}
rec("choice sanity set overall", correct >= 7, `${correct}/${choiceCases.length} correct`);

const noulCases: { name: string; state: unknown; instructions: string; high: boolean }[] = [
  { name: "escalate: legal threat", state: "Customer threatens legal action and demands a full refund today.", instructions: "Should a human follow up?", high: true },
  { name: "escalate: resolved", state: "Customer says thanks, the issue is resolved and they need nothing else.", instructions: "Should a human follow up?", high: false },
  { name: "shipped: delivered", state: { order: "A-1", status: "delivered" }, instructions: "Has the order shipped?", high: true },
  { name: "shipped: cancelled", state: { order: "A-2", status: "cancelled before fulfilment" }, instructions: "Has the order shipped?", high: false },
];
let nulOk = 0;
for (const c of noulCases) {
  const r = await decide(c.state, { q: { type: "noul", instructions: c.instructions } });
  const p = r.json?.answers?.q?.noul;
  const ok = r.status === 200 && typeof p === "number" && p >= 0 && p <= 1 && (c.high ? p > 0.5 : p < 0.5);
  if (ok) nulOk++;
  rec(`noul sanity: ${c.name}`, ok, `p(true) = ${typeof p === "number" ? p.toFixed(3) : "?"}`);
}
rec("noul sanity set overall", nulOk >= 3, `${nulOk}/${noulCases.length}`);

const scoreCases = [
  { name: "urgency: outage", state: "Production database is down and every customer is affected.", expectTop: "high" },
  { name: "urgency: typo", state: "There is a small typo in the website footer.", expectTop: "low" },
];
for (const c of scoreCases) {
  const r = await decide(c.state, { u: { type: "score", instructions: "How urgent is this issue?", criteria: ["low", "medium", "high"] } });
  const a = r.json?.answers?.u;
  const structure = !!a && "score" in a && !!a.probabilities && typeof a.confidence === "number";
  const probsOk = a?.probabilities ? Math.abs(sum(a.probabilities) - 1) < 1e-3 : false;
  // Probability keys are level indices; the legend maps them to the names.
  const topIndex = a?.probabilities ? argmax(a.probabilities) : "?";
  const top = a?.legend?.[topIndex] ?? topIndex;
  rec(`score: ${c.name}`, r.status === 200 && structure && !!a?.legend && probsOk && top === c.expectTop, `score ${typeof a?.score === "number" ? a.score.toFixed(3) : a?.score}, most likely ${top}, probs sum ${a?.probabilities ? sum(a.probabilities).toFixed(4) : "?"}`);
}

// ---- 5. Several questions in one call; consistency; concurrency --------------------------
{
  const r = await decide(overdueState, {
    status: invoiceQ.status,
    urgency: { type: "score", criteria: ["low", "medium", "high"] },
    escalate: { type: "noul", instructions: "Should a human follow up?" },
  });
  const a = r.json?.answers;
  rec("three question types in one request", r.status === 200 && !!a?.status?.choice && !!a?.urgency?.probabilities && typeof a?.escalate?.noul === "number", `${r.status}`);
}
{
  const runs = [];
  for (let i = 0; i < 3; i++) runs.push((await decide(overdueState, invoiceQ)).json?.answers?.status?.probabilities);
  const same = runs.every((p) => p && Object.keys(p).every((k) => Math.abs(p[k] - runs[0][k]) < 1e-6));
  rec("same request repeated 3 times gives identical probabilities", same, JSON.stringify(runs[0]));
}
{
  const warm: number[] = [];
  for (let i = 0; i < 6; i++) warm.push((await decide(overdueState, invoiceQ)).ms);
  rec("warm single-question latency", median(warm) < 3000, `median ${median(warm).toFixed(0)} ms (min ${Math.min(...warm).toFixed(0)}, max ${Math.max(...warm).toFixed(0)})`);
  const par = await Promise.all(Array.from({ length: 4 }, () => decide(overdueState, invoiceQ)));
  const allOk = par.every((r) => r.status === 200);
  const same = par.every((r) => r.json?.answers?.status?.choice === par[0].json?.answers?.status?.choice);
  rec("4 concurrent requests all succeed with the same answer", allOk && same, par.map((r) => r.status).join(","));
}

// ---- 6. Image input -------------------------------------------------------------------
{
  const crcTable = new Uint32Array(256).map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
  const crc = (b: Buffer) => { let c = 0xffffffff; for (const x of b) c = crcTable[(c ^ x) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  const chunk = (type: string, data: Buffer) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
  const solid = (r: number, g: number, b: number, n = 64) => {
    const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(n, 0); ihdr.writeUInt32BE(n, 4); ihdr[8] = 8; ihdr[9] = 2;
    const row = Buffer.concat([Buffer.from([0]), Buffer.from(Array.from({ length: n }, () => [r, g, b]).flat())]);
    const raw = Buffer.concat(Array.from({ length: n }, () => row));
    const png = Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", ihdr), chunk("IDAT", deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
    return "data:image/png;base64," + png.toString("base64");
  };
  const colorQ = { c: { type: "choice", instructions: "What is the dominant color of the image?", criteria: { red: null, green: null, blue: null } } };
  const red = await decide("Look at the attached image.", colorQ, { images: [solid(220, 20, 20)] });
  const blue = await decide("Look at the attached image.", colorQ, { images: [solid(20, 20, 220)] });
  rec("image input: red image is called red", red.status === 200 && red.json?.answers?.c?.choice === "red", `${red.status} ${red.json?.answers?.c?.choice ?? red.text.slice(0, 80)}`);
  rec("image input: blue image is called blue", blue.status === 200 && blue.json?.answers?.c?.choice === "blue", `${blue.status} ${blue.json?.answers?.c?.choice ?? blue.text.slice(0, 80)}`);
}

// ---- 7. Context size ---------------------------------------------------------------------
{
  const filler = "The quarterly report discusses routine operational matters in considerable detail. ";
  const long = (n: number) => filler.repeat(n);
  const mid = { invoice: "INV-55", notes: long(70) + "NOTE: this invoice has been paid in full. " + long(70) };
  const r = await decide(mid, invoiceQ);
  rec("about 3k-token state is handled", r.status === 200, `${r.status}, input_tokens ${r.json?.usage?.input_tokens}, answer ${r.json?.answers?.status?.choice}`);
  const big = await decide({ notes: long(2500) }, invoiceQ);
  rec("oversized state is rejected as context_length_exceeded (400)", big.status === 400 && big.json?.error?.code === "context_length_exceeded" && big.json?.error?.param === "state",
    `${big.status} ${big.json?.error?.code ?? ""}: ${(big.json?.error?.message ?? big.text).toString().slice(0, 90)}`);
  const after = await decide(overdueState, invoiceQ);
  rec("gateway answers normally after the oversized request", after.status === 200 && after.json?.answers?.status?.choice === "overdue");
}

// ---- 8. Validation and access -----------------------------------------------------------------
{
  const v = async (name: string, body: unknown, want: (s: number) => boolean) => {
    const r = await post("/v1/systemone", body);
    rec(name, want(r.status), `${r.status} ${(r.json?.error?.code ?? "").toString()}`);
  };
  await v("missing state is rejected (400)", { model: MODEL, questions: invoiceQ }, (s) => s === 400);
  await v("unknown question type is rejected (400)", { model: MODEL, state: "x", questions: { q: { type: "ranking", criteria: ["a", "b"] } } }, (s) => s === 400);
  await v("choice with one option is rejected (400)", { model: MODEL, state: "x", questions: { q: { type: "choice", criteria: { only: null } } } }, (s) => s === 400);
  await v("score with 11 levels is rejected (400)", { model: MODEL, state: "x", questions: { q: { type: "score", criteria: Array.from({ length: 11 }, (_, i) => `l${i}`) } } }, (s) => s === 400);
  await v("empty questions are rejected (400)", { model: MODEL, state: "x", questions: {} }, (s) => s === 400);
  await v("unknown model is rejected (4xx)", { model: "no-such-model", state: "x", questions: invoiceQ }, (s) => s >= 400 && s < 500);
  const bad = await post("/v1/systemone", "{not json");
  rec("malformed JSON is rejected (400)", bad.status === 400, `${bad.status}`);
  const chat = await post("/v1/chat/completions", { model: MODEL, messages: [{ role: "user", content: "hi" }], max_tokens: 4 }, okKey);
  const scopeHint = (s: number) => (s === 403 ? " (LOCALBASE_QUALIFY_KEY needs inference:chat for this check)" : "");
  rec("chat completions on a decision model are refused with an explanation", chat.status === 400 && /decision/i.test(chat.text), `${chat.status} ${(chat.json?.error?.message ?? "").slice(0, 80)}${scopeHint(chat.status)}`);
  const emb = await post("/v1/embeddings", { model: MODEL, input: "hi" }, okKey);
  rec("embeddings on a decision model are refused (400)", emb.status === 400, `${emb.status} ${(emb.json?.error?.code ?? "").toString()}${scopeHint(emb.status)}`);
  if (noScopeKey) {
    const noScope = await post("/v1/systemone", { model: MODEL, state: "x", questions: invoiceQ }, noScopeKey);
    rec("key without inference:decision is refused (403)", noScope.status === 403, `${noScope.status}`);
  } else {
    console.log("SKIP  key without inference:decision is refused (set LOCALBASE_QUALIFY_NOSCOPE_KEY)");
  }
  const noKey = await post("/v1/systemone", { model: MODEL, state: "x", questions: invoiceQ }, null);
  rec("no key is refused (401)", noKey.status === 401, `${noKey.status}`);
}

const passed = rows.filter((r) => r.pass).length;
console.log(`\nSUMMARY ${passed}/${rows.length} checks passed; cold first request ${(coldMs / 1000).toFixed(1)} s`);
console.log("FAILED: " + (rows.filter((r) => !r.pass).map((r) => r.check).join(" | ") || "none"));
process.exit(rows.every((r) => r.pass) ? 0 : 1);
