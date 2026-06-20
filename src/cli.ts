import { parseFlag } from "./utils/args";
import { printHelp } from "./domains/app/commands/help";
import { runConfigure } from "./domains/config/commands/configure";
import { runInit } from "./domains/config/commands/init";
import { runDoctor } from "./domains/system/commands/doctor";
import { runCatalog } from "./domains/models/commands/catalog";
import { runRecommend } from "./domains/models/commands/recommend";
import { runInstalled } from "./domains/models/commands/installed";
import { runInstall } from "./domains/models/commands/install";
import { runServe } from "./domains/runtime/commands/serve";
import { runKeys } from "./domains/auth/commands/keys";
import { runReset } from "./domains/maintenance/commands/reset";
import { runUninstall } from "./domains/maintenance/commands/uninstall";
import { createAppContext } from "./context";

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
  const command = args[0];

  if (command === "--help" || command === "-h") {
    printHelp();
    return 0;
  }

  const ctx = createAppContext(args);

  if (!command || command.startsWith("--")) {
    return runConfigure(args, ctx);
  }

  if (command === "init") return runInit(args, ctx);
  if (command === "configure") return runConfigure(args, ctx);
  if (command === "doctor") return runDoctor(args, ctx);
  if (command === "catalog") return runCatalog(args, ctx);
  if (command === "recommend") return runRecommend(args, ctx);

  if (command === "reset") return runReset(args, ctx);
  if (command === "uninstall") return runUninstall(args, ctx);

  if (command === "keys") return runKeys(args, ctx);
  if (command === "installed") return runInstalled(args, ctx);
  if (command === "install") return runInstall(args, ctx);
  if (command === "serve") return runServe(args, ctx);

  console.error(`Unknown command: ${command}`);
  printHelp();
  return 2;
}

try {
  const code = await main();
  process.exit(code);
} catch (error) {
  console.error(`Error: ${(error as Error).message}`);
  process.exit(1);
}
