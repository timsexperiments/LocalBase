import { migrationAssetSource, migrationAssetsPath } from "./migration-assets";

await Bun.write(migrationAssetsPath, await migrationAssetSource());
