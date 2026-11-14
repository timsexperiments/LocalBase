export function leadingLegalCommentBlocks(source: string): string[] {
  const blocks: string[] = [];
  let remainder = source;
  while (true) {
    const match = /^\s*(\/\*[\s\S]*?\*\/)/.exec(remainder);
    if (!match) break;
    const block = match[1]!;
    if (
      /copyright|redistribution|permission|license|public domain/i.test(block)
    )
      blocks.push(block);
    remainder = remainder.slice(match[0].length);
  }
  return blocks;
}
