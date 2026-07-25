import {
  parseArgs as parseCittyArgs,
  renderUsage,
  type ArgDef,
  type ArgsDef,
} from "citty";
import type { AppContext } from "../../../context";
import { globalOptionsSchema, type GlobalOptions } from "./inputs";
import {
  commands,
  configureCommand,
  globalArgs,
  groupForPath,
  rootCommand,
  type CittyCommand,
  type Command,
} from "./command-tree";
import { createCommandOutput, type CommandOutput } from "./output";
import type { CommandResult } from "./output";
import { CliInputError, formatZodError, toCliInputError } from "./errors";

export { CliInputError } from "./errors";
export type { CommandExecution } from "./command-tree";

type ResolvedCommand =
  | {
      kind: "command";
      command: Command;
      input: unknown;
      global: GlobalOptions;
      parent: CittyCommand;
    }
  | {
      kind: "help";
      command: CittyCommand;
      parent?: CittyCommand;
      global: GlobalOptions;
    }
  | { kind: "version"; global: GlobalOptions }
  | {
      kind: "error";
      message: string;
      command?: CittyCommand;
      parent?: CittyCommand;
      global?: GlobalOptions;
    };

function camelCase(value: string): string {
  return value.replace(/-([a-z])/g, (_, letter: string) =>
    letter.toUpperCase(),
  );
}

function commandForPath(path: string[]): Command | undefined {
  return commands.find(
    (command) =>
      command.path.length === path.length &&
      command.path.every((part, index) => part === path[index]),
  );
}

function validateOptionSyntax(rawArgs: string[], argsDef: ArgsDef): void {
  const flags = new Map<string, ArgDef>();
  for (const [name, definition] of Object.entries(argsDef)) {
    if (definition.type === "positional") continue;
    flags.set(`--${name}`, definition);
    const aliases =
      "alias" in definition && definition.alias
        ? Array.isArray(definition.alias)
          ? definition.alias
          : [definition.alias]
        : [];
    for (const alias of aliases) flags.set(`-${alias}`, definition);
  }

  let literal = false;
  for (const token of rawArgs) {
    if (token === "--") {
      literal = true;
      continue;
    }
    if (literal || !token.startsWith("-")) continue;
    const equals = token.indexOf("=");
    const name = equals === -1 ? token : token.slice(0, equals);
    const valueProvided = equals !== -1;
    if (name.startsWith("--no-")) {
      const definition = flags.get(`--${name.slice(5)}`);
      if (!definition || definition.type !== "boolean") {
        throw new CliInputError(`Unknown option: ${name}`);
      }
      if (valueProvided) {
        throw new CliInputError(`${name} does not accept a value`);
      }
      continue;
    }
    const definition = flags.get(name);
    if (!definition) throw new CliInputError(`Unknown option: ${name}`);
    if (definition.type === "boolean" && valueProvided) {
      throw new CliInputError(`${name} does not accept a value`);
    }
  }
}

function normalizeArgs(
  input: Record<string, unknown>,
): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(input)
      .filter(([key]) => key !== "_")
      .map(([key, value]) => [camelCase(key), value]),
  );
}

function splitGlobalOptions(rawArgs: string[]): {
  global: GlobalOptions;
  args: string[];
} {
  const args: string[] = [];
  const globalInput: string[] = [];
  let literal = false;
  for (let index = 0; index < rawArgs.length; index += 1) {
    const token = rawArgs[index];
    if (token === "--") literal = true;
    if (literal) {
      args.push(token);
      continue;
    }
    if (
      token === "--non-interactive" ||
      token.startsWith("--non-interactive=")
    ) {
      globalInput.push(token);
      continue;
    }
    if (token === "--json" || token.startsWith("--json=")) {
      globalInput.push(token);
      continue;
    }
    if (token === "--root" || token.startsWith("--root=")) {
      globalInput.push(token);
      if (token === "--root" && index + 1 < rawArgs.length) {
        globalInput.push(rawArgs[index + 1]);
        index += 1;
      }
      continue;
    }
    args.push(token);
  }
  validateOptionSyntax(globalInput, globalArgs);
  const result = globalOptionsSchema.safeParse(
    normalizeArgs(parseCittyArgs(globalInput, globalArgs)),
  );
  if (!result.success) throw new CliInputError(formatZodError(result.error));
  return { global: result.data, args };
}

function validatePositionals(command: Command, positionals: string[]): void {
  const { minimum = 0, maximum = 0 } = command.positionals ?? {};
  if (positionals.length < minimum || positionals.length > maximum) {
    throw new CliInputError(
      `Invalid positional arguments for ${command.path.join(" ")}`,
    );
  }
}

function findPath(args: string[]): { path: string[]; consumed: number } {
  const path: string[] = [];
  for (const token of args) {
    if (token === "--" || token.startsWith("-")) break;
    const candidate = [...path, token];
    if (commandForPath(candidate) || groupForPath(candidate)) path.push(token);
    else break;
  }
  return { path, consumed: path.length };
}

function hasHelpFlag(args: string[]): boolean {
  for (const token of args) {
    if (token === "--") return false;
    if (token === "--help" || token === "-h") return true;
  }
  return false;
}

function hasExplicitJsonFlag(args: string[]): boolean {
  for (const token of args) {
    if (token === "--") return false;
    if (token === "--json") return true;
  }
  return false;
}

function parentFor(command: Command): CittyCommand {
  return command.path.length === 2
    ? groupForPath(command.path.slice(0, -1))!
    : rootCommand;
}

export async function resolveCli(rawArgs: string[]): Promise<ResolvedCommand> {
  let usageCommand: CittyCommand = rootCommand;
  let usageParent: CittyCommand | undefined;
  const explicitJson = hasExplicitJsonFlag(rawArgs);
  let global: GlobalOptions | undefined = explicitJson
    ? { json: true, nonInteractive: true }
    : undefined;
  try {
    const split = splitGlobalOptions(rawArgs);
    const { args } = split;
    global = split.global;
    if (args.length === 1 && ["--version", "-v"].includes(args[0])) {
      return { kind: "version", global };
    }
    const { path, consumed } = findPath(args);
    const command = commandForPath(path);
    const group = groupForPath(path);
    if (hasHelpFlag(args)) {
      if (command) {
        return {
          kind: "help",
          command: command.citty,
          parent: parentFor(command),
          global,
        };
      }
      if (group) return { kind: "help", command: group, global };
      if (path.length === 0)
        return { kind: "help", command: rootCommand, global };
    }
    if (group) {
      const groupArgs = args.slice(consumed);
      if (groupArgs.length === 0)
        return { kind: "help", command: group, global };
      usageCommand = group;
      validateOptionSyntax(groupArgs, {});
      return {
        kind: "error",
        message: `Unknown command: ${groupArgs[0]}`,
        command: group,
        global,
      };
    }
    const defaultCommand =
      path.length === 0 && (!args[0] || args[0].startsWith("-"))
        ? configureCommand
        : undefined;
    const resolved = command ?? defaultCommand;
    if (!resolved) {
      return {
        kind: "error",
        message: `Unknown command: ${args[consumed] ?? args[0] ?? ""}`,
        command: rootCommand,
        global,
      };
    }
    usageCommand = resolved.citty;
    usageParent = parentFor(resolved);
    return {
      kind: "command",
      command: resolved,
      input: parseCommandInput(
        resolved,
        command ? args.slice(consumed) : args,
        global,
      ),
      global,
      parent: usageParent,
    };
  } catch (error) {
    const inputError = toCliInputError(error);
    return {
      kind: "error",
      message:
        inputError?.message ??
        (error instanceof Error ? error.message : String(error)),
      command: usageCommand,
      parent: usageParent,
      global,
    };
  }
}

export function parseCommandInput(
  command: Command,
  rawArgs: string[],
  global: GlobalOptions,
): unknown {
  validateOptionSyntax(rawArgs, command.args ?? {});
  const parsed = parseCittyArgs(rawArgs, command.args ?? {});
  const positionals = Array.isArray(parsed._) ? parsed._ : [];
  validatePositionals(command, positionals);
  const input = command.parse(normalizeArgs(parsed), positionals);
  return command.validate?.(input, global) ?? input;
}

export async function commandHelpText(
  command: CittyCommand,
  parent?: CittyCommand,
): Promise<string> {
  const usage = await renderUsage(command, parent);
  const examples = commands.find((entry) => entry.citty === command)?.examples;
  return [
    usage,
    examples?.length
      ? `EXAMPLES\n\n${examples.map((example) => `  ${example}`).join("\n")}`
      : "",
  ]
    .filter(Boolean)
    .join("\n\n");
}

export async function printCommandHelp(
  command: CittyCommand,
  parent?: CittyCommand,
): Promise<void> {
  console.log(await commandHelpText(command, parent));
}

export function rootCommandDefinition(): CittyCommand {
  return rootCommand;
}

export async function executeCommand(
  command: Command,
  input: unknown,
  global: GlobalOptions,
  context: AppContext,
  output: CommandOutput = createCommandOutput(global.json),
): Promise<CommandResult> {
  const result = await command.run(input, context, {
    global,
    output,
  });
  return { ...result, data: command.resultSchema.parse(result.data) };
}
