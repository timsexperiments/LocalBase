// Decisions (DESIGN §22.17): the pure part. Reads an answer in words, builds
// the request and its code, and writes the batch export. No page access, so
// tools/jstest can run it.

// The words come from how far the top answer is ahead. These cuts were measured
// on one model and are provisional (docs/decision-calibration-2026-11-01.json):
// another model gets its own row here once it has been measured.
const CLEF_FLASH = { strong: 0.7, close: 0.25, unsure: 0.5 };
export const BANDS = { "clef-flash-q4_k_m": CLEF_FLASH, default: CLEF_FLASH };

export const BAND_WORDS = { strong: "Strongly favored", leaning: "Leaning", close: "Close call", unsure: "Unsure" };

export const STANDING_LINE = "These are the model’s odds, not a check that it is right. Judgment calls can still come back lopsided.";

/** What a close result asks of the reader. Keyed to the band table, not to any number here. */
export const BAND_NOTES = { close: "Neither is far ahead, so this one is worth a person’s look." };

export const MAX_ROWS = 200;
export const MAX_OPTIONS = { choice: 255, score: 10 };

/** Which words fit a top answer and its distance from the next one. */
export function band(top, second, model) {
  const cuts = BANDS[model] ?? BANDS.default;
  const gap = top - second;
  if (top < cuts.unsure) return "unsure";
  if (gap >= cuts.strong) return "strong";
  if (gap < cuts.close) return "close";
  return "leaning";
}

/**
 * The one shape for all three question types. rows keep the order to show: a
 * scale keeps its own order, the rest go from most to least likely.
 */
export function readAnswer(answer, model) {
  let rows;
  if (answer?.type === "noul" || typeof answer?.noul === "number") {
    rows = [{ label: "Yes", p: answer.noul }, { label: "No", p: 1 - answer.noul }];
  } else if (answer?.legend) {
    rows = Object.entries(answer.probabilities ?? {}).map(([i, p]) => ({ label: String(answer.legend[i] ?? i), p }));
  } else {
    rows = Object.entries(answer?.probabilities ?? {}).map(([label, p]) => ({ label, p }));
  }
  if (!rows.length) return null;
  const ranked = [...rows].sort((a, b) => b.p - a.p);
  const [first, next = { label: "", p: 0 }] = ranked;
  if (!answer?.legend) rows = ranked;
  const id = band(first.p, next.p, model);
  const close = id === "close";
  return {
    band: id,
    word: BAND_WORDS[id],
    answer: first.label,
    verdict: close ? `${first.label} or ${next.label}` : first.label,
    odds: close ? `${percent(first.p)} and ${percent(next.p)}` : percent(first.p),
    top: first,
    note: BAND_NOTES[id] ?? "",
    // In a close result both answers lead, because the verdict names both.
    rows: rows.map((r) => ({ ...r, top: r.label === first.label || (close && r.label === next.label), text: percent(r.p) })),
  };
}

/** Whole numbers, and never a bare 0 % or 100 % for something the model left open. */
export function percent(p) {
  const n = Math.round(p * 100);
  if (p > 0 && n < 1) return "under 1%";
  if (p < 1 && n > 99) return "over 99%";
  return `${n}%`;
}

export const needsReview = (read) => read?.band === "close" || read?.band === "unsure";

/** Options as the API wants them, leaving out empty rows. */
export function namedOptions(options) {
  return options.map((o) => ({ name: o.name.trim(), note: o.note?.trim() ?? "" })).filter((o) => o.name);
}

/** What is wrong with a form, as { field, message }, or null. */
export function problem(form) {
  if (!form.question.trim()) return { field: "question", message: "Ask a question first." };
  if (form.type !== "noul") {
    const names = namedOptions(form[form.type]).map((o) => o.name);
    const word = form.type === "score" ? "levels" : "options";
    if (names.length < 2) return { field: "options", message: `Add at least two ${word}.` };
    if (names.length > MAX_OPTIONS[form.type]) return { field: "options", message: `Use at most ${MAX_OPTIONS[form.type]} ${word}.` };
    if (new Set(names).size < names.length) return { field: "options", message: `Each of the ${word} needs its own name.` };
  }
  if (form.batch) {
    const rows = situations(form.situation).length;
    if (!rows) return { field: "situation", message: "Add at least one situation, one per line." };
    if (rows > MAX_ROWS) return { field: "situation", message: `Use at most ${MAX_ROWS} situations at a time. This has ${rows}.` };
  }
  return null;
}

/** The request for one situation, exactly what the API takes. */
export function buildRequest(form, situation, model) {
  const question = { type: form.type, instructions: form.question.trim() };
  if (form.type === "choice") question.criteria = Object.fromEntries(namedOptions(form.choice).map((o) => [o.name, o.note || null]));
  if (form.type === "score") question.criteria = namedOptions(form.score).map((o) => o.name);
  const body = { model, questions: { answer: question } };
  if (situation?.trim()) body.state = situation.trim();
  return body;
}

export function curlSnippet(body, origin) {
  const json = JSON.stringify(body, null, 2).replaceAll("'", "'\\''");
  return `curl ${origin}/v1/systemone \\\n  -H "Authorization: Bearer $LOCALBASE_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '${json}'`;
}

export function jsSnippet(body, origin) {
  const json = JSON.stringify(body, null, 2).replaceAll("\n", "\n  ");
  return `const response = await fetch(${JSON.stringify(`${origin}/v1/systemone`)}, {\n  method: "POST",\n  headers: {\n    Authorization: \`Bearer \${process.env.LOCALBASE_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json}),\n});\nconst { answers } = await response.json();\nconsole.log(answers.answer);`;
}

/** Batch input: one situation per line. A line a spreadsheet quoted is unquoted. */
export function situations(text) {
  return text
    .replace(/^﻿/, "")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => (/^".*"$/.test(line) ? line.slice(1, -1).replaceAll('""', '"') : line));
}

/** A spreadsheet runs a cell starting with these as a formula. */
export function csvCell(value) {
  let text = String(value ?? "");
  if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
  return /[",\r\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
}

export function batchCSV(rows) {
  const labels = [];
  for (const row of rows) for (const r of row.read?.rows ?? []) if (!labels.includes(r.label)) labels.push(r.label);
  const lines = [["situation", "answer", "read", ...labels, "error"]];
  for (const row of rows) {
    const odds = new Map((row.read?.rows ?? []).map((r) => [r.label, r.p.toFixed(4)]));
    lines.push([row.situation, row.read?.answer ?? "", row.read?.word ?? "", ...labels.map((l) => odds.get(l) ?? ""), row.error ?? ""]);
  }
  return lines.map((line) => line.map(csvCell).join(",")).join("\r\n") + "\r\n";
}

// Starting points. Filling one never runs it.
export const TEMPLATES = [
  { id: "route", name: "Route a ticket", type: "choice", question: "Which team should handle this ticket?", situation: "Customer: I was charged twice for the same order.",
    options: [{ name: "billing", note: "Charges, refunds, invoices" }, { name: "shipping", note: "Delivery and tracking" }, { name: "technical", note: "Bugs and crashes" }] },
  { id: "tone", name: "Read a review", type: "choice", question: "What is the sentiment?", situation: "Review: It works, and setup took two minutes. The manual is thin.",
    options: [{ name: "positive", note: "" }, { name: "negative", note: "" }, { name: "neutral", note: "" }] },
  { id: "followup", name: "Needs a person?", type: "noul", question: "Should a person follow up?", situation: "Customer says thanks, the issue is resolved and they need nothing else." },
  { id: "scam", name: "Spot a scam", type: "noul", question: "Is this message a scam?", situation: "Message: You won a free cruise. Click this link and enter your card number." },
  { id: "urgent", name: "How urgent", type: "score", question: "How urgent is this issue?", situation: "The search page is slow for some customers this morning.",
    options: [{ name: "low", note: "" }, { name: "medium", note: "" }, { name: "high", note: "" }] },
  { id: "stars", name: "Rate a review", type: "score", question: "How would you rate this review?", situation: "Review: Good value, though delivery took a while.",
    options: ["very bad", "bad", "okay", "good", "very good"].map((name) => ({ name, note: "" })) },
];
