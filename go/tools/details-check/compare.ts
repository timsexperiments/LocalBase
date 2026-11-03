// Compares two tools/details-check.sh results. Reports any page where the same <details> lands differently, or where only
// one browser found it, and any <details> set to display: contents. Exits 1 on any of these.
const [a, b] = await Promise.all(process.argv.slice(2, 4).map(async (f) => JSON.parse(await Bun.file(f).text())));
console.log(`A: ${a.version}, ::details-content ${a.supports}\nB: ${b.version}, ::details-content ${b.supports}`);
let same = 0, diff = 0, fragile = 0;
const near = (x: any, y: any, by = 4) => x === y || (typeof x === "number" && typeof y === "number" && Math.abs(x - y) <= by);
for (const ra of a.rows) {
  const rb = b.rows.find((r: any) => r.page === ra.page && r.width === ra.width);
  if (ra.count || rb?.count) console.log(`${String(ra.width).padStart(4)} ${ra.page}: ${ra.count} and ${rb?.count ?? "?"} found`);
  if (!rb || ra.count !== rb.count) { diff++; console.log(`  DIFFERENT COUNT: ${ra.count} vs ${rb?.count}`); continue; }
  ra.list.forEach((x: any, i: number) => {
    const y = rb.list[i];
    const fields = ["cls", "display", "content", "inside"].filter((k) => x[k] !== y[k]).concat(["detailsW", "summaryLeft", "summaryW", "below", "contentLeft", "contentW", "overflow"].filter((k) => !near(x[k], y[k])));
    if (fields.length) { diff++; console.log(`  DIFFERENT "${x.label}" (${x.cls}): ` + fields.map((k) => `${k} ${x[k]} vs ${y[k]}`).join(", ")); } else same++;
    if (x.display === "contents" || y.display === "contents") { fragile++; console.log(`  DISPLAY CONTENTS "${x.label}" (${x.cls}): boxed differently by browsers with ::details-content`); }
    if (x.content && (x.below < -1 || !x.inside)) console.log(`  OFF in A: "${x.label}" below=${x.below} inside=${x.inside}`);
    if (y.content && (y.below < -1 || !y.inside)) console.log(`  OFF in B: "${y.label}" below=${y.below} inside=${y.inside}`);
  });
}
console.log(`${same} the same, ${diff} different, ${fragile} display: contents`);
process.exit(diff || fragile ? 1 : 0);
