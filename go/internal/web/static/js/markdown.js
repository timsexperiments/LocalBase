// Markdown for model replies, the GitHub-flavoured subset chat models use:
// paragraphs, headings, fenced and inline code, emphasis, strikethrough,
// links, lists, blockquotes, rules and pipe tables. parse() returns plain
// objects (tested in go/tools/jstest); build() turns them into DOM nodes
// with textContent only, never innerHTML. Raw HTML stays literal text, and
// links are kept only for http, https and mailto.

const fence = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/;
const heading = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/;
const rule = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const quote = /^ {0,3}> ?(.*)$/;
const item = /^( {0,3})([-*+]|\d{1,9}[.)])([ \t]+(.*)|$)/;
const tableSeparator = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/;

const blank = (line) => /^\s*$/.test(line);

function cells(line) {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const out = [];
  let cur = "";
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "\\" && s[i + 1] === "|") {
      cur += "|";
      i++;
    } else if (s[i] === "|") {
      out.push(cur.trim());
      cur = "";
    } else cur += s[i];
  }
  out.push(cur.trim());
  return out;
}

function startsBlock(line, next) {
  return fence.test(line) || heading.test(line) || rule.test(line) || quote.test(line) || item.test(line) ||
    (line.includes("|") && next !== undefined && tableSeparator.test(next) && cells(line).length === cells(next).length);
}

/** Parses markdown into blocks. */
export function parse(text) {
  return blocks(text.replace(/\r\n?/g, "\n").split("\n"));
}

function blocks(lines) {
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (blank(line)) {
      i++;
      continue;
    }
    let m;
    if ((m = fence.exec(line))) {
      const marker = m[1];
      const indent = line.length - line.trimStart().length;
      const body = [];
      i++;
      while (i < lines.length && !new RegExp(`^ {0,3}${marker[0]}{${marker.length},}\\s*$`).test(lines[i])) {
        body.push(lines[i].slice(Math.min(indent, lines[i].length - lines[i].trimStart().length)));
        i++;
      }
      i++; // closing fence (or the end, while a reply is still streaming)
      out.push({ t: "code", lang: m[2] || "", text: body.join("\n") });
      continue;
    }
    if ((m = heading.exec(line))) {
      out.push({ t: "h", level: m[1].length, c: inline(m[2] ?? "") });
      i++;
      continue;
    }
    if (rule.test(line)) {
      out.push({ t: "hr" });
      i++;
      continue;
    }
    if (quote.test(line)) {
      const body = [];
      while (i < lines.length && !blank(lines[i]) && (quote.test(lines[i]) || body.length)) {
        const q = quote.exec(lines[i]);
        if (!q && startsBlock(lines[i], lines[i + 1])) break;
        body.push(q ? q[1] : lines[i]);
        i++;
      }
      out.push({ t: "quote", c: blocks(body) });
      continue;
    }
    if ((m = item.exec(line))) {
      const ordered = /\d/.test(m[2]);
      const list = { t: "list", ordered, start: ordered ? parseInt(m[2], 10) : 1, loose: false, items: [] };
      while (i < lines.length) {
        const im = item.exec(lines[i]);
        if (!im || /\d/.test(im[2]) !== ordered) break;
        const width = im[1].length + im[2].length + Math.max(1, Math.min(4, (im[3] || " ").length - (im[4] || "").length));
        const body = [im[4] || ""];
        i++;
        while (i < lines.length) {
          const l = lines[i];
          if (blank(l)) {
            // A blank line continues the item only if indented content follows.
            if (i + 1 < lines.length && lines[i + 1].length - lines[i + 1].trimStart().length >= width && !blank(lines[i + 1])) {
              list.loose = true; // a blank line between an item's blocks
              body.push("");
              i++;
              continue;
            }
            break;
          }
          const indent = l.length - l.trimStart().length;
          if (indent >= width) body.push(l.slice(width));
          else if (!item.exec(l) && !startsBlock(l, lines[i + 1]) && !blank(body[body.length - 1])) body.push(l.trimStart());
          else break;
          i++;
        }
        list.items.push(blocks(body));
        while (i < lines.length && blank(lines[i]) && i + 1 < lines.length && item.exec(lines[i + 1])) {
          list.loose = true; // a blank line between items
          i++;
        }
      }
      out.push(list);
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && tableSeparator.test(lines[i + 1])) {
      const head = cells(line);
      const seps = cells(lines[i + 1]);
      if (head.length === seps.length) {
        const align = seps.map((s) => (s.startsWith(":") && s.endsWith(":") ? "center" : s.endsWith(":") ? "right" : s.startsWith(":") ? "left" : ""));
        const rows = [];
        i += 2;
        while (i < lines.length && !blank(lines[i]) && lines[i].includes("|")) {
          const r = cells(lines[i]);
          rows.push(head.map((_, k) => inline(r[k] ?? "")));
          i++;
        }
        out.push({ t: "table", align, head: head.map((h) => inline(h)), rows });
        continue;
      }
    }
    const para = [line];
    i++;
    while (i < lines.length && !blank(lines[i]) && !startsBlock(lines[i], lines[i + 1])) {
      para.push(lines[i]);
      i++;
    }
    out.push({ t: "p", c: inline(para.join("\n")) });
  }
  return out;
}

const safeHref = (href) => /^(https?:\/\/|mailto:)/i.test(href);

const inlinePatterns = [
  ["code", /(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)/],
  ["escape", /\\([!-/:-@[-`{-~])/],
  ["autolink", /<((?:https?:\/\/|mailto:)[^\s<>]+)>/],
  ["link", /\[((?:[^\[\]\\]|\\.)*)\]\(\s*<?([^\s()<>]*(?:\([^\s()]*\)[^\s()<>]*)*)>?(?:\s+"[^"]*")?\s*\)/],
  ["url", /https?:\/\/[^\s<>]*[^\s<>.,;:!?'")\]]/],
  ["strong", /\*\*(?=\S)([\s\S]*?\S)\*\*|(?<![\p{L}\p{N}_])__(?=\S)([\s\S]*?\S)__(?![\p{L}\p{N}_])/u],
  ["em", /\*(?=[^\s*])([\s\S]*?[^\s*])\*(?!\*)|(?<![\p{L}\p{N}_])_(?=[^\s_])([\s\S]*?[^\s_])_(?![\p{L}\p{N}_])/u],
  ["del", /~~(?=\S)([\s\S]*?\S)~~/],
  ["br", / {2,}(?=\n)|\\(?=\n)/],
];

/** Parses inline markdown. */
export function inline(text) {
  const out = [];
  const pushText = (v) => {
    if (!v) return;
    const last = out[out.length - 1];
    if (last && last.t === "text") last.v += v;
    else out.push({ t: "text", v });
  };
  let rest = text;
  while (rest) {
    let best = null;
    for (const [kind, re] of inlinePatterns) {
      const m = re.exec(rest);
      if (m && (!best || m.index < best.m.index)) best = { kind, m };
    }
    if (!best) {
      pushText(rest);
      break;
    }
    const { kind, m } = best;
    pushText(rest.slice(0, m.index));
    rest = rest.slice(m.index + m[0].length);
    switch (kind) {
      case "code":
        out.push({ t: "code", v: m[2].replace(/\n/g, " ").replace(/^ (.*) $/, "$1") });
        break;
      case "escape":
        pushText(m[1]);
        break;
      case "autolink":
        out.push({ t: "link", href: m[1], c: [{ t: "text", v: m[1] }] });
        break;
      case "link":
        if (safeHref(m[2])) out.push({ t: "link", href: m[2], c: inline(m[1]) });
        else out.push(...inline(m[1]));
        break;
      case "url":
        out.push({ t: "link", href: m[0], c: [{ t: "text", v: m[0] }] });
        break;
      case "strong":
        out.push({ t: "strong", c: inline(m[1] ?? m[2]) });
        break;
      case "em":
        out.push({ t: "em", c: inline(m[1] ?? m[2]) });
        break;
      case "del":
        out.push({ t: "del", c: inline(m[1]) });
        break;
      case "br":
        out.push({ t: "br" });
        break;
    }
  }
  return out;
}

// --- DOM ---------------------------------------------------------------------------

function el(tag, className, children) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  for (const child of children ?? []) node.append(child);
  return node;
}

function inlineNodes(list) {
  return list.map((n) => {
    switch (n.t) {
      case "text":
        return document.createTextNode(n.v);
      case "code":
        return Object.assign(el("code"), { textContent: n.v });
      case "strong":
        return el("strong", "", inlineNodes(n.c));
      case "em":
        return el("em", "", inlineNodes(n.c));
      case "del":
        return el("del", "", inlineNodes(n.c));
      case "br":
        return el("br");
      case "link": {
        const a = el("a", "", inlineNodes(n.c));
        a.href = n.href;
        a.rel = "noopener noreferrer";
        a.target = "_blank";
        return a;
      }
    }
    return document.createTextNode("");
  });
}

/**
 * Builds DOM for parsed blocks. codeBlock(lang, text) makes a code block's
 * element (the page adds its Copy button).
 */
export function build(list, codeBlock) {
  return list.map((b) => {
    switch (b.t) {
      case "p":
        return el("p", "", inlineNodes(b.c));
      case "h":
        return el(`h${Math.min(6, b.level + 2)}`, "", inlineNodes(b.c)); // replies sit under the page's h1/h2
      case "hr":
        return el("hr");
      case "code":
        return codeBlock(b.lang, b.text);
      case "quote":
        return el("blockquote", "", build(b.c, codeBlock));
      case "list": {
        // Tight lists show their paragraphs inline, as CommonMark renders them.
        const itemNodes = (it) => (!b.loose && it.length >= 1 && it[0].t === "p" ? [...inlineNodes(it[0].c), ...build(it.slice(1), codeBlock)] : build(it, codeBlock));
        const list = el(b.ordered ? "ol" : "ul", "", b.items.map((it) => el("li", "", itemNodes(it))));
        if (b.ordered && b.start !== 1) list.start = b.start;
        return list;
      }
      case "table": {
        const cell = (tag, c, k) => {
          const td = el(tag, "", inlineNodes(c));
          if (b.align[k]) td.style.textAlign = b.align[k];
          return td;
        };
        const table = el("table", "", [
          el("thead", "", [el("tr", "", b.head.map((h, k) => cell("th", h, k)))]),
          el("tbody", "", b.rows.map((r) => el("tr", "", r.map((c, k) => cell("td", c, k))))),
        ]);
        return el("div", "md-table", [table]); // scrolls on its own, never the page
      }
    }
    return document.createTextNode("");
  });
}
