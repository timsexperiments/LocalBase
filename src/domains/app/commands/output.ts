import { z } from "zod";

const jsonValueSchema = z.json();

export const commandSuccessSchema = z
  .object({ ok: z.literal(true), data: jsonValueSchema })
  .strict();

export const commandErrorSchema = z
  .object({
    ok: z.literal(false),
    error: z
      .object({ code: z.string().min(1), message: z.string().min(1) })
      .strict(),
  })
  .strict();

const enabledModalitiesSchema = z
  .object({ llm: z.boolean(), stt: z.boolean(), image: z.boolean() })
  .strict();

export const serveLifecycleEventSchema = z.discriminatedUnion("event", [
  z
    .object({
      event: z.literal("started"),
      baseUrl: z.string().url(),
      enabled: enabledModalitiesSchema,
    })
    .strict(),
  z
    .object({ event: z.literal("stopped"), exitCode: z.number().int().min(0) })
    .strict(),
  z
    .object({
      event: z.literal("error"),
      error: z
        .object({ code: z.string().min(1), message: z.string().min(1) })
        .strict(),
    })
    .strict(),
]);

export type ServeLifecycleEvent = z.infer<typeof serveLifecycleEventSchema>;

export type CommandResult = {
  data: unknown;
  exitCode?: number;
};

export interface CommandOutput {
  info(message: string): void;
  error(message: string): void;
  lifecycle(event: ServeLifecycleEvent): void;
}

function writeStdout(value: unknown): void {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

export function writeJsonSuccess(data: unknown): void {
  writeStdout(commandSuccessSchema.parse({ ok: true, data }));
}

export function writeJsonError(code: string, message: string): void {
  writeStdout(
    commandErrorSchema.parse({ ok: false, error: { code, message } }),
  );
}

export function createCommandOutput(json = false): CommandOutput {
  return {
    info(message) {
      if (json) console.error(message);
      else console.log(message);
    },
    error(message) {
      console.error(message);
    },
    lifecycle(event) {
      if (json) writeStdout(serveLifecycleEventSchema.parse(event));
    },
  };
}
