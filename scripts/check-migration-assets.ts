import { readdirSync } from "node:fs";

const expected = readdirSync("drizzle")
  .filter((name) => /^\d+_.+\.sql$/.test(name))
  .sort()
  .map((name) => `../../drizzle/${name}`);
const assets = await Bun.file("src/db/migration-assets.ts").text();

if (!assets.includes("../../drizzle/meta/_journal.json")) {
  throw new Error(
    "src/db/migration-assets.ts is missing the migration journal; run bun run db:generate.",
  );
}

for (const path of expected) {
  if (!assets.includes(path)) {
    throw new Error(
      `src/db/migration-assets.ts is missing ${path}; run bun run db:generate.`,
    );
  }
}
