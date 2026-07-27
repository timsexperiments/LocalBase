import { z } from "zod";
import {
  createServiceManagerFixtureRunner,
  runManagedGatewayFixture,
} from "./service-manager-fixture";

const platform = z
  .enum(["darwin", "linux"])
  .parse(process.env.LOCALBASE_TEST_PLATFORM);
Object.defineProperty(process, "platform", { value: platform });

const args = Bun.argv.slice(2);
if (
  process.env.LOCALBASE_TEST_MANAGED_GATEWAY === "1" ||
  process.env.LOCALBASE_TEST_FOREGROUND_GATEWAY === "1"
) {
  process.exit(await runManagedGatewayFixture(args));
}

const { setServiceManagerCommandRunnerForTests } =
  await import("../domains/service/manager");
const commandTimeout = z.coerce
  .number()
  .int()
  .min(25)
  .max(5_000)
  .default(2_000)
  .parse(process.env.LOCALBASE_TEST_SERVICE_COMMAND_TIMEOUT_MS);
setServiceManagerCommandRunnerForTests(
  createServiceManagerFixtureRunner(),
  commandTimeout,
);

const { BACKEND_GUARDIAN_COMMAND, runBackendGuardian } =
  await import("../domains/runtime/backend-guardian");
if (args[0] === BACKEND_GUARDIAN_COMMAND) {
  process.exit(await runBackendGuardian(args.slice(1)));
}

const { runCli } = await import("../domains/app/commands/runner");
const code = await runCli(
  args,
  async (options, initializeDatabase, initializeUnderOperationLock) => {
    const { createAppContext } = await import("../context");
    return await createAppContext(
      options,
      initializeDatabase,
      initializeUnderOperationLock,
    );
  },
);
process.exit(code);
