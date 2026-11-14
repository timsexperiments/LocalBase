export function readmeLicenseSection(readme: string): string | undefined {
  const lines = readme.split(/(?<=\n)/);
  const start = lines.findIndex((line) =>
    /^#{1,6}\s+(?:the\s+)?license\b/i.test(line.trim()),
  );
  if (start < 0) return undefined;
  const startLevel = /^#+/.exec(lines[start]!.trim())![0].length;
  let end = lines.length;
  for (let index = start + 1; index < lines.length; index += 1) {
    const heading = /^(#+)\s/.exec(lines[index]!.trim());
    if (heading && heading[1]!.length <= startLevel) {
      end = index;
      break;
    }
  }
  const section = lines.slice(start, end).join("");
  return section.trim().length ? section : undefined;
}
