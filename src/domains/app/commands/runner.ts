import { printHelp } from "./help";
import { commandRegistry } from "./registry";
import type { AppContext } from "../../../context";
import type { CLICommand, CommandFlag } from "./types";
import { z } from "zod";

type CommandResolution =
  | { kind: "help" }
  | { kind: "error"; message: string }
  | { kind: "command"; command: CLICommand };

const nonEmptyValue = z.string().min(1);
const flagValueSchemas: Record<string, z.ZodType<string>> = {
  boolean: z.never(),
  "true|false": z.enum(["true", "false"]),
  "llm|stt|image": z.enum(["llm", "stt", "image"]),
  "bearer|x-api-key|either": z.enum(["bearer", "x-api-key", "either"]),
  "n|auto": z.union([z.literal("auto"), z.string().regex(/^[1-4]$/)]),
  n: z.string().regex(/^\d+$/),
  port: z.string().regex(/^\d+$/),
  tokens: z.string().regex(/^\d+$/),
  gb: z.string().regex(/^\d+$/),
};

function schemaFor(flag: CommandFlag): z.ZodType<string> {
  return flagValueSchemas[flag.type] ?? nonEmptyValue;
}

function matchesPositionals(command: CLICommand, count: number): boolean {
  const positionals = command.positional ?? [];
  if (
    positionals.some(
      (positional) =>
        positional.endsWith("...]") || positional.endsWith("...>"),
    )
  ) {
    return (
      count >=
      positionals.filter((positional) => positional.startsWith("<")).length
    );
  }
  const required = positionals.filter((positional) =>
    positional.startsWith("<"),
  ).length;
  return count >= required && count <= positionals.length;
}

function validateInvocation(
  command: CLICommand,
  values: string[],
): string | undefined {
  const flags = new Map<string, CommandFlag>();
  for (const flag of command.flags ?? []) {
    flags.set(flag.name, flag);
    if (flag.short) flags.set(`-${flag.short}`, flag);
  }

  const positionals: string[] = [];
  const seen = new Set<string>();
  for (let index = 0; index < values.length; index += 1) {
    const token = values[index];
    if (!token.startsWith("-")) {
      positionals.push(token);
      continue;
    }

    const equalsIndex = token.indexOf("=");
    const flagName = equalsIndex === -1 ? token : token.slice(0, equalsIndex);
    const inlineValue =
      equalsIndex === -1 ? undefined : token.slice(equalsIndex + 1);
    const flag = flags.get(flagName);
    if (!flag)
      return `Unknown flag for ${command.name || "configure"}: ${flagName}`;
    if (seen.has(flag.name))
      return `Flag may only be provided once: ${flag.name}`;
    seen.add(flag.name);

    if (flag.type === "boolean") {
      if (inlineValue !== undefined)
        return `${flag.name} does not accept a value`;
      continue;
    }

    const value = inlineValue ?? values[++index];
    if (value === undefined) return `Missing value for ${flag.name}`;
    if (!schemaFor(flag).safeParse(value).success) {
      return `Invalid value for ${flag.name}: ${value}`;
    }
  }

  if (!matchesPositionals(command, positionals.length)) {
    const expected = command.positional?.join(" ") ?? "no positional arguments";
    return `Invalid positional arguments for ${command.name || "configure"}; expected ${expected}`;
  }
  return undefined;
}

/** Resolves and validates a command before any configuration or hardware probing. */
export function resolveCommand(args: string[]): CommandResolution {
  if (args.includes("--help") || args.includes("-h")) return { kind: "help" };

  const first = args[0];
  let command: CLICommand | undefined;
  let commandParts = 0;
  if (!first || first.startsWith("-")) {
    command = commandRegistry.find((entry) => entry.name === "");
  } else {
    for (const entry of commandRegistry) {
      if (!entry.name) continue;
      const parts = entry.name.split(" ");
      if (
        parts.length > commandParts &&
        parts.every((part, index) => args[index] === part)
      ) {
        command = entry;
        commandParts = parts.length;
      }
    }
  }

  if (!command) return { kind: "error", message: `Unknown command: ${first}` };
  const validationError = validateInvocation(command, args.slice(commandParts));
  return validationError
    ? { kind: "error", message: validationError }
    : { kind: "command", command };
}

function reportResolution(
  resolution: Exclude<CommandResolution, { kind: "command" }>,
): number {
  if (resolution.kind === "help") {
    printHelp();
    return 0;
  }
  console.error(`Error: ${resolution.message}`);
  return 2;
}

/**
 * Dispatches already-validated CLI arguments with a constructed application context.
 */
export async function runRegistry(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const resolution = resolveCommand(args);
  if (resolution.kind !== "command") return reportResolution(resolution);
  return await resolution.command.handler(args, ctx);
}

/** Routes help and argument failures before invoking the potentially expensive context factory. */
export async function runCli(
  args: string[],
  createContext: (
    args: string[],
    initializeDatabase: boolean,
  ) => Promise<AppContext>,
): Promise<number> {
  const resolution = resolveCommand(args);
  if (resolution.kind !== "command") return reportResolution(resolution);
  const ctx = await createContext(
    args,
    resolution.command.requiresDatabase ?? true,
  );
  try {
    return await resolution.command.handler(args, ctx);
  } finally {
    ctx.database.close();
  }
}
