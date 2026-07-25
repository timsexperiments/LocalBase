import type { AppContext } from "../../../context";
import {
  commandHelpText,
  executeCommand,
  printCommandHelp,
  resolveCli,
  rootCommandDefinition,
} from "./framework";
import type { GlobalOptions } from "./inputs";
import { toCliInputError } from "./errors";
import {
  createCommandOutput,
  writeJsonError,
  writeJsonSuccess,
} from "./output";

type CreateContext = (
  options: GlobalOptions,
  initializeDatabase: boolean,
) => Promise<AppContext>;

function errorCode(error: unknown): string {
  return toCliInputError(error) ? "invalid_input" : "operational_error";
}

async function reportError(
  message: string,
  command?: Parameters<typeof commandHelpText>[0],
  parent?: Parameters<typeof commandHelpText>[1],
  json = false,
): Promise<number> {
  if (json) writeJsonError("invalid_input", message);
  console.error(`Error: ${message}`);
  if (command) console.error(await commandHelpText(command, parent));
  return 2;
}

/** Routes legacy diagnostics to stderr while a JSON command owns stdout. */
async function withJsonStdoutGuard<T>(
  enabled: boolean,
  work: () => Promise<T>,
): Promise<T> {
  if (!enabled) return await work();
  const originalLog = console.log;
  console.log = (...values: unknown[]) => console.error(...values);
  try {
    return await work();
  } finally {
    console.log = originalLog;
  }
}

/** Resolves CLI input before creating a context or probing host hardware. */
export async function runCli(
  args: string[],
  createContext: CreateContext,
): Promise<number> {
  const resolution = await resolveCli(args);
  if (resolution.kind === "error") {
    return await reportError(
      resolution.message,
      resolution.command,
      resolution.parent,
      resolution.global?.json,
    );
  }
  if (resolution.kind === "version") {
    const meta = rootCommandDefinition().meta;
    const resolvedMeta = await (typeof meta === "function" ? meta() : meta);
    const version = resolvedMeta?.version ?? "0.1.0";
    if (resolution.global.json) writeJsonSuccess({ version });
    else console.log(version);
    return 0;
  }
  if (resolution.kind === "help") {
    if (resolution.global.json) {
      writeJsonSuccess({
        help: await commandHelpText(resolution.command, resolution.parent),
      });
    } else {
      await printCommandHelp(resolution.command, resolution.parent);
    }
    return 0;
  }

  const { command, global } = resolution;
  const output = createCommandOutput(global.json);
  let context: AppContext | undefined;
  try {
    context = await createContext(global, command.requiresDatabase ?? true);
    const result = await withJsonStdoutGuard(global.json, () =>
      executeCommand(command, resolution.input, global, context!, output),
    );
    const exitCode = result.exitCode ?? 0;
    if (global.json && !command.longRunning) writeJsonSuccess(result.data);
    return exitCode;
  } catch (error) {
    const inputError = toCliInputError(error);
    const message =
      inputError?.message ??
      (error instanceof Error ? error.message : String(error));
    if (command.longRunning && global.json) {
      output.lifecycle({
        event: "error",
        error: { code: errorCode(error), message },
      });
      return inputError ? 2 : 1;
    }
    if (inputError) {
      return await reportError(
        inputError.message,
        command.citty,
        resolution.parent,
        global.json,
      );
    }
    if (global.json) writeJsonError("operational_error", message);
    console.error(`Error: ${message}`);
    return 1;
  } finally {
    context?.database.close();
  }
}
