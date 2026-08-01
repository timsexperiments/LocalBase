import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { ServiceInput } from "../../app/commands/inputs";
import { serviceLifecycleResultSchema } from "../../app/commands/results";
import {
  getServiceInspection,
  restartService,
  startService,
  stopService,
  type ServiceInspection,
} from "../manager";

function printStatus(
  execution: CommandExecution,
  inspection: ServiceInspection,
): void {
  const { service, gateway } = inspection;
  execution.output.info(`Service: ${service.state} (${service.manager})`);
  execution.output.info(
    `Manager: ${service.managerAvailable ? (service.managerState ?? "unknown") : "unavailable"}`,
  );
  execution.output.info(
    `Definition: ${service.definitionInstalled ? "installed" : "not installed"} (${service.definitionPath})`,
  );
  execution.output.info(
    `Process: ${service.pid === null ? "not running" : `pid ${service.pid}${service.uptimeSeconds === null ? "" : `, uptime ${service.uptimeSeconds}s`}`}`,
  );
  execution.output.info(
    `Restarts: ${service.restartCount === null ? "unavailable" : service.restartCount}`,
  );
  execution.output.info(
    `Gateway: ${gateway.state}${gateway.state === "ready" ? "" : ` (${gateway.detail})`}`,
  );
}

async function runManagedStart(
  action: "start" | "restart",
  ctx: AppContext,
  execution: CommandExecution,
) {
  const inspection =
    action === "start"
      ? await startService(ctx.config.root)
      : await restartService(ctx.config.root);
  if (
    inspection.service.state !== "running" &&
    inspection.service.state !== "starting"
  ) {
    throw new Error(
      `LocalBase service manager reported ${inspection.service.state} after ${action}.`,
    );
  }
  execution.output.info(
    `${action === "start" ? "Started" : "Restarted"} LocalBase service with ${inspection.service.manager}.`,
  );
  printStatus(execution, inspection);
  return serviceLifecycleResultSchema.parse(inspection);
}

export async function runStart(
  _input: ServiceInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  return { data: await runManagedStart("start", ctx, execution) };
}

export async function runRestart(
  _input: ServiceInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  return { data: await runManagedStart("restart", ctx, execution) };
}

export async function runStop(
  _input: ServiceInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const inspection = await stopService(ctx.config.root);
  execution.output.info(
    `Stopped and disabled LocalBase service managed by ${inspection.service.manager}.`,
  );
  return {
    data: serviceLifecycleResultSchema.parse(inspection),
  };
}

export async function runStatus(
  _input: ServiceInput,
  ctx: AppContext,
  execution: CommandExecution,
) {
  const inspection = await getServiceInspection(ctx.config.root);
  printStatus(execution, inspection);
  return { data: serviceLifecycleResultSchema.parse(inspection) };
}
