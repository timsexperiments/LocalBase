import type { MinimalAppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { LogsInput } from "../../app/commands/inputs";
import {
  createLogEvent,
  followLogEvents,
  formatHumanLogEvent,
  readLogSnapshot,
  type LogEvent,
  type LogFilters,
} from "../logging";

function filtersFrom(input: LogsInput): LogFilters {
  return {
    ...(input.since ? { since: input.since } : {}),
    ...(input.level ? { level: input.level } : {}),
    ...(input.runtime ? { runtime: input.runtime } : {}),
    ...(input.requestId ? { requestId: input.requestId } : {}),
  };
}

async function writeEvent(
  event: LogEvent,
  execution: CommandExecution,
): Promise<void> {
  if (execution.global.json) {
    if (!process.stdout.write(`${JSON.stringify(event)}\n`)) {
      await new Promise<void>((resolve) =>
        process.stdout.once("drain", resolve),
      );
    }
    return;
  }
  execution.output.info(formatHumanLogEvent(event));
}

/** Reads or follows the root-bound JSONL log stream without opening SQLite. */
export async function runLogs(
  input: LogsInput,
  ctx: MinimalAppContext,
  execution: CommandExecution,
): Promise<{ data: { events: LogEvent[] }; exitCode?: number }> {
  const filters = filtersFrom(input);
  if (!input.follow) {
    const events = await readLogSnapshot(ctx.config.root, filters, input.limit);
    if (!execution.global.json) {
      for (const event of events) await writeEvent(event, execution);
    }
    return { data: { events } };
  }

  const controller = new AbortController();
  const stop = () => controller.abort();
  process.once("SIGINT", stop);
  process.once("SIGTERM", stop);
  try {
    try {
      await followLogEvents(
        ctx.config.root,
        filters,
        async (event) => await writeEvent(event, execution),
        controller.signal,
      );
    } catch (error) {
      if (!execution.global.json) throw error;
      await writeEvent(
        createLogEvent({
          severity: "error",
          eventName: "logging.follow-failed",
          category: "logging",
          component: "logger",
          runtime: "cli",
          message: error instanceof Error ? error.message : String(error),
        }),
        execution,
      );
      return { data: { events: [] }, exitCode: 1 };
    }
  } finally {
    process.off("SIGINT", stop);
    process.off("SIGTERM", stop);
  }
  return { data: { events: [] } };
}
