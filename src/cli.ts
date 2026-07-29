import { runCli } from "./domains/app/commands/runner";
import {
  BACKEND_GUARDIAN_COMMAND,
  runBackendGuardian,
} from "./domains/runtime/backend-guardian";
import { redactExternalLogText } from "./domains/observability/logging";

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
  if (args[0] === BACKEND_GUARDIAN_COMMAND) {
    return await runBackendGuardian(args.slice(1));
  }
  return await runCli(
    args,
    async (options, initializeDatabase, initializeUnderOperationLock) => {
      const { createAppContext } = await import("./context");
      return await createAppContext(
        options,
        initializeDatabase,
        initializeUnderOperationLock,
      );
    },
    async (options) => {
      const { createMinimalAppContext } = await import("./context");
      return createMinimalAppContext(options);
    },
  );
}

try {
  const code = await main();
  process.exit(code);
} catch (error) {
  console.error(`Error: ${redactExternalLogText((error as Error).message)}`);
  process.exit(1);
}
