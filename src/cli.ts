import { createAppContext } from "./context";
import { runRegistry } from "./domains/app/commands/runner";
import {
  BACKEND_GUARDIAN_COMMAND,
  runBackendGuardian,
} from "./domains/runtime/backend-guardian";

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
  if (args[0] === BACKEND_GUARDIAN_COMMAND) {
    return await runBackendGuardian(args.slice(1));
  }
  const ctx = await createAppContext(args);
  return await runRegistry(args, ctx);
}

try {
  const code = await main();
  process.exit(code);
} catch (error) {
  console.error(`Error: ${(error as Error).message}`);
  process.exit(1);
}
