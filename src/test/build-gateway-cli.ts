import { join } from "node:path";
import { buildUi } from "../../scripts/build-ui";

const outfile = process.argv[2];
if (!outfile) throw new Error("Expected a gateway fixture output path.");
await buildUi();
const result = await Bun.build({
  entrypoints: [join(import.meta.dir, "../cli.ts")],
  target: "bun",
  naming: { asset: "[dir]/[name].[ext]" },
  compile: { outfile, autoloadDotenv: false, autoloadBunfig: false },
  plugins: [
    {
      name: "gateway-fixture-memory",
      setup(build) {
        build.onResolve({ filter: /\/host-memory-provider$/ }, () => ({
          path: join(import.meta.dir, "gateway-memory-provider.ts"),
        }));
      },
    },
  ],
});
if (!result.success)
  throw new Error(result.logs.map((log) => log.message).join("\n"));
