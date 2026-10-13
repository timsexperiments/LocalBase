import { leadingLegalCommentBlocks } from "./leading-legal-comment-blocks";

const bunCommit = "0d9b296af33f2b851fcbf4df3e9ec89751734ba4";
const libarchiveCommit = "ded82291ab41d5e355831b96b0e1ff49e24d8939";
const buildUrl = `https://raw.githubusercontent.com/oven-sh/bun/${bunCommit}/scripts/build/deps/libarchive.ts`;
const buildResponse = await fetch(buildUrl);
if (!buildResponse.ok)
  throw new Error(
    `Could not fetch libarchive build list: ${buildResponse.status}.`,
  );
const buildSource = await buildResponse.text();
const sourceList = /const SOURCES = \[([\s\S]*?)\];/.exec(buildSource)?.[1];
if (!sourceList)
  throw new Error("Could not read the pinned libarchive source list.");
const sourceNames = [...sourceList.matchAll(/"([^"]+)"/g)].map(
  (match) => match[1]!,
);
const blocks = new Set<string>();
for (let offset = 0; offset < sourceNames.length; offset += 8) {
  const batch = sourceNames.slice(offset, offset + 8);
  const sources = await Promise.all(
    batch.map(async (name) => {
      const url = `https://raw.githubusercontent.com/libarchive/libarchive/${libarchiveCommit}/libarchive/${name}.c`;
      const response = await fetch(url);
      if (!response.ok)
        throw new Error(`Could not fetch ${name}.c: ${response.status}.`);
      return response.text();
    }),
  );
  for (const source of sources)
    for (const block of leadingLegalCommentBlocks(source)) blocks.add(block);
}
const header = `libarchive headers from compiled sources at ${libarchiveCommit}. Complete distinct leading comment blocks containing legal notices are reproduced with trailing whitespace removed.\n`;
const notices = [...blocks]
  .map((block) => block.replace(/[\t ]+$/gm, ""))
  .sort()
  .join("\n\n");
await Bun.write(
  "scripts/release-notices/native/libarchive-compiled-headers.txt",
  `${header}\n${notices}\n`,
);
