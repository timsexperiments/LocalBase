import { printHelp } from "./domains/app/commands/help";
import { commandRegistry } from "./domains/app/commands/registry";
import { createAppContext } from "./context";

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
  const command = args[0];

  if (command === "--help" || command === "-h" || args.includes("--help") || args.includes("-h")) {
    printHelp();
    return 0;
  }

  const ctx = createAppContext(args);

  // Default fallback to interactive configuration if no command is specified or if it starts with an option flag
  if (!command || command.startsWith("--")) {
    const defaultCmd = commandRegistry.find((cmd) => cmd.name === "");
    if (!defaultCmd) {
      console.error("Default configuration command not found in registry");
      return 1;
    }
    return await defaultCmd.handler(args, ctx);
  }

  // Search for the longest matching command path in the registry (supporting subcommands like "keys create")
  let matchedCmd = null;
  let maxParts = 0;

  for (const cmd of commandRegistry) {
    if (!cmd.name) continue;
    const parts = cmd.name.split(" ");
    const matches = parts.every((part, i) => args[i] === part);
    if (matches && parts.length > maxParts) {
      matchedCmd = cmd;
      maxParts = parts.length;
    }
  }

  if (matchedCmd) {
    return await matchedCmd.handler(args, ctx);
  }

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
