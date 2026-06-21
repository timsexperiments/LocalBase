import { commandRegistry } from "./registry";
import type { CLICommand } from "./types";

function buildUsageLine(cmd: CLICommand): string {
  const parts = ["local-base"];
  if (cmd.name) {
    parts.push(cmd.name);
  }
  if (cmd.positional) {
    parts.push(...cmd.positional);
  }
  if (cmd.flags) {
    for (const flag of cmd.flags) {
      if (flag.type === "boolean") {
        parts.push(`[${flag.name}]`);
      } else {
        parts.push(`[${flag.name} <${flag.type}>]`);
      }
    }
  }
  return parts.join(" ");
}

/**
 * Dynamically prints help text for the application by parsing the commandRegistry.
 */
export function printHelp(): void {
  console.log("local-base - Bun TypeScript local AI installer/manager\n");
  console.log("Usage:");
  
  for (const cmd of commandRegistry) {
    console.log(`  ${buildUsageLine(cmd)}`);
  }
  
  console.log("\nCommands:");
  const activeCommands = commandRegistry.filter(cmd => cmd.name !== "");
  const maxLen = Math.max(...activeCommands.map(cmd => cmd.name.length));
  
  for (const cmd of activeCommands) {
    const padded = cmd.name.padEnd(maxLen + 2, " ");
    console.log(`  ${padded}${cmd.description}`);
  }
  
  console.log("");
}
