import { printHelp } from "./help";
import { commandRegistry } from "./registry";
import type { AppContext } from "../../../context";

/**
 * Parses and routes the CLI arguments dynamically using the command registry.
 * Falls back to printing the help screen on invalid commands or flags.
 */
export async function runRegistry(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const command = args[0];

  if (
    command === "--help" ||
    command === "-h" ||
    args.includes("--help") ||
    args.includes("-h")
  ) {
    printHelp();
    return 0;
  }

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
