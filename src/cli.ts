import { createAppContext } from "./context";
import { runRegistry } from "./domains/app/commands/runner";

async function main(): Promise<number> {
  const args = Bun.argv.slice(2);
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
