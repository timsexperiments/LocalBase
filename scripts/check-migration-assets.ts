import { migrationAssetSource, migrationAssetsPath } from "./migration-assets";

const file = Bun.file(migrationAssetsPath);
if (!(await file.exists())) {
  throw new Error(`${migrationAssetsPath} is missing; run bun run db:prepare.`);
}
if ((await file.text()) !== (await migrationAssetSource())) {
  throw new Error(`${migrationAssetsPath} is stale; run bun run db:prepare.`);
}
