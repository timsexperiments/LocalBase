import { loadConfig } from "./manager";
import { detectSpecs } from "./system";
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

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
  const command = args[0];

  if (command === "--help" || command === "-h") {
    printHelp();
    return 0;
  }

  if (!command || command.startsWith("--")) {
    return runConfigure(args);
  }

  if (command === "init") return runInit(args);
  if (command === "configure") return runConfigure(args);
  if (command === "doctor") return runDoctor(args);
  if (command === "catalog") return runCatalog(args);
  if (command === "recommend") return runRecommend(args);

  if (command === "reset") return runReset(args);
  if (command === "uninstall") return runUninstall(args);

  const root = parseFlag(args, "--root");
  const config = loadConfig(root, detectSpecs().gpuVramGb);

  if (command === "keys") return runKeys(args, config);
  if (command === "installed") return runInstalled(args, config);
  if (command === "install") return runInstall(args, config);
  if (command === "serve") return runServe(args, config);

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
