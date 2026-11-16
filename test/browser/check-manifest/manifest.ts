// "All pass" has to mean every check ran. A check inside a guard that quietly stays off prints nothing, so a run can
// pass without it. This compares what should have run with what did. See README.md.

export type Allow = { mode: string; words: string; why: string };

export type Result = {
  checks: number;
  lines: number;
  missing: string[];
  allowed: string[];
  unverifiable: string[];
  unparsed: string[];
};

export function parseAllow(text: string): Allow[] {
  return text.split("\n").filter((l) => l.trim() && !l.startsWith("#")).map((l) => {
    const [mode, words, why] = l.split("\t");
    return { mode, words, why };
  });
}

// A check's message is its last argument: the string before the closing bracket, on the line that ends the call.
const ending = /,\s*(?:`([^`]*)`|"((?:[^"\\]|\\.)*)")\s*\)\s*;?\s*\}*\s*$/;

export function messagesIn(source: string): { messages: string[]; unparsed: string[] } {
  const lines = source.split("\n"), messages: string[] = [], unparsed: string[] = [];
  lines.forEach((line, n) => {
    if (!/(^|[\s;{(])check\(/.test(line) || /const check\b|function check\b/.test(line)) return;
    if (/^\s*(\/\/|\*)/.test(line)) return;
    for (let k = n; k < Math.min(n + 8, lines.length); k++) {
      const m = ending.exec(lines[k]);
      if (m) { messages.push(m[1] ?? m[2]); return; }
    }
    unparsed.push(`line ${n + 1}: ${line.trim().slice(0, 90)}`);
  });
  return { messages, unparsed };
}

// What a message's result line looks like: its words as they are, and anything at all for each ${...}.
export function toPattern(message: string): string {
  let out = "", i = 0;
  while (i < message.length) {
    if (message[i] === "$" && message[i + 1] === "{") {
      let depth = 1, quote = "";
      for (i += 2; i < message.length && depth > 0; i++) {
        const c = message[i];
        if (quote) { if (c === "\\") i++; else if (c === quote) quote = ""; continue; }
        if (c === '"' || c === "'") quote = c;
        else if (c === "{") depth++;
        else if (c === "}") depth--;
      }
      if (!out.endsWith(".*")) out += ".*";
      continue;
    }
    const c = message[i] === "\\" ? message[++i] : message[i];
    out += c.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&");
    i++;
  }
  return out;
}

export function compare(source: string, log: string, allow: Allow[] = [], mode = ""): Result {
  const { messages, unparsed } = messagesIn(source);
  const results = log.split("\n").filter((l) => /^(ok|FAIL) /.test(l));
  const unique = [...new Set(messages)];
  const out: Result = { checks: unique.length, lines: results.length, missing: [], allowed: [], unverifiable: [], unparsed };
  for (const message of unique) {
    const pattern = toPattern(message);
    // With no words of its own it would match anything, so it can't be held to account.
    if (pattern.replace(/\.\*/g, "").length < 4) { out.unverifiable.push(message); continue; }
    const rx = new RegExp(`^(ok|FAIL) +${pattern}`);
    if (results.some((l) => rx.test(l))) continue;
    const a = allow.find((x) => (x.mode === mode || x.mode === "*") && message.includes(x.words));
    if (a) out.allowed.push(`${message.slice(0, 70)} (${a.why})`);
    else out.missing.push(message);
  }
  return out;
}

// For scripts that print one line per page, width and scheme: every combination must appear.
export function missingLabels(expected: string[], log: string): string[] {
  const lines = log.split("\n");
  return expected.filter((label) => {
    const rx = new RegExp(`^(ok|FAIL|note) +${label.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&")}( |:|$)`);
    return !lines.some((l) => rx.test(l));
  });
}

export function phoneLabels(source: string): string[] {
  const block = /const pages\s*=\s*\[([\s\S]*?)\]/.exec(source)?.[1] ?? "";
  const pages = [...block.matchAll(/"([^"]+)"/g)].map((m) => m[1]);
  return [360, 390, 430, 1280].flatMap((w) => pages.map((p) => `${String(w).padStart(4)} ${p}`));
}

export function axeLabels(source: string): string[] {
  const states = [...source.matchAll(/^\s*\["([a-z0-9-]+)", "\/app/gm)].map((m) => m[1]);
  return [1280, 390].flatMap((w) => ["light", "dark"].flatMap((s) => states.map((n) => `${String(w).padStart(4)} ${s.padEnd(5)} ${n}`)));
}
