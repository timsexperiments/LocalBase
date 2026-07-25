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

type CreateContext = (
  options: GlobalOptions,
  initializeDatabase: boolean,
) => Promise<AppContext>;

async function reportError(
  message: string,
  command?: Parameters<typeof commandHelpText>[0],
  parent?: Parameters<typeof commandHelpText>[1],
): Promise<number> {
  console.error(`Error: ${message}`);
  if (command) console.error(await commandHelpText(command, parent));
  return 2;
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
    );
  }
  if (resolution.kind === "version") {
    const meta = rootCommandDefinition().meta;
    const resolvedMeta = await (typeof meta === "function" ? meta() : meta);
    console.log(resolvedMeta?.version ?? "0.1.0");
    return 0;
  }
  if (resolution.kind === "help") {
    await printCommandHelp(resolution.command, resolution.parent);
    return 0;
  }

  let context: AppContext | undefined;
  try {
    context = await createContext(
      resolution.global,
      resolution.command.requiresDatabase ?? true,
    );
    return await executeCommand(
      resolution.command,
      resolution.input,
      resolution.global,
      context,
    );
  } catch (error) {
    const inputError = toCliInputError(error);
    if (inputError) {
      return await reportError(
        inputError.message,
        resolution.command.citty,
        resolution.parent,
      );
    }
    console.error(
      `Error: ${error instanceof Error ? error.message : String(error)}`,
    );
    return 1;
  } finally {
    context?.database.close();
  }
}
