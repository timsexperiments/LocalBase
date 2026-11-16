// bun test/browser/check-manifest/run.ts check <run.ts> <log> [--mode=<name>] [--allow=<file>]
// bun test/browser/check-manifest/run.ts phone <run.ts> <log>
// bun test/browser/check-manifest/run.ts axe <run.ts> <log>
// Exits 1 when anything that should have run didn't.
import { axeLabels, compare, missingLabels, parseAllow, phoneLabels } from "./manifest.ts";

const args = process.argv.slice(2);
const [kind, source, log] = args.filter((a) => !a.startsWith("--"));
if (!kind || !source || !log) { console.error("usage: run.ts check|phone|axe <run.ts> <log> [--mode=<name>] [--allow=<file>]"); process.exit(2); }
const sourceText = await Bun.file(source).text();
const logText = await Bun.file(log).text();
if (kind === "check") {
  const mode = args.find((a) => a.startsWith("--mode="))?.slice(7) ?? "";
  const allowFile = args.find((a) => a.startsWith("--allow="))?.slice(8);
  const r = compare(sourceText, logText, allowFile ? parseAllow(await Bun.file(allowFile).text()) : [], mode);
  console.log(`manifest: ${r.checks} checks in the source, ${r.lines} result lines, ${r.missing.length} missing, ${r.allowed.length} allowed absent, ${r.unverifiable.length} unverifiable, ${r.unparsed.length} unparsed`);
  for (const a of r.allowed) console.log(`  allowed absent: ${a}`);
  for (const u of r.unverifiable) console.log(`  unverifiable: ${u.slice(0, 90)}`);
  for (const m of r.missing) console.log(`  MISSING: ${m.slice(0, 130)}`);
  for (const u of r.unparsed) console.log(`  UNPARSED: ${u}`);
  process.exit(r.missing.length || r.unparsed.length ? 1 : 0);
}
const expected = kind === "phone" ? phoneLabels(sourceText) : kind === "axe" ? axeLabels(sourceText) : [];
if (!expected.length) { console.log(`manifest ${kind}: nothing expected, so this can't vouch for the run`); process.exit(1); }
const missing = missingLabels(expected, logText);
console.log(`manifest ${kind}: ${expected.length} expected, ${expected.length - missing.length} found, ${missing.length} missing`);
for (const m of missing.slice(0, 20)) console.log(`  MISSING: ${m}`);
process.exit(missing.length ? 1 : 0);
