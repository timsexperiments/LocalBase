import { stat } from "node:fs/promises";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import { CliInputError } from "../../app/commands/errors";
import type {
  VoicesAddInput,
  VoicesListInput,
  VoicesRemoveInput,
} from "../../app/commands/inputs";
import {
  VOICE_CONSENT_STATEMENT,
  VOICE_MAX_UPLOAD_BYTES,
  VoiceError,
  type CustomVoice,
} from "../voice-contract";
import { createCustomVoiceStore } from "../voice-store";

/** Voices created from the CLI are attributed to the local operator. */
const CLI_CREATOR = "cli";

function store(ctx: AppContext) {
  return createCustomVoiceStore(
    ctx.database.get(ctx.config.root),
    ctx.config.root,
  );
}

function describe(voice: CustomVoice): string {
  return `${voice.id} | ${voice.name} | ${(voice.durationMs / 1000).toFixed(1)} s | created=${voice.createdAt} | by=${voice.createdBy ?? "unknown"}`;
}

async function asCliError<T>(operation: () => Promise<T> | T): Promise<T> {
  try {
    return await operation();
  } catch (error) {
    if (error instanceof VoiceError) throw new CliInputError(error.message);
    throw error;
  }
}

export function runVoicesList(
  _input: VoicesListInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { voices: CustomVoice[] } } {
  const voices = store(ctx).list();
  if (voices.length === 0) {
    execution.output.info(
      "No custom voices. Add one with: local-base voices add clip.wav --name <name> --consent",
    );
  }
  for (const voice of voices) execution.output.info(describe(voice));
  return { data: { voices } };
}

export async function runVoicesAdd(
  input: VoicesAddInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { voice: CustomVoice } }> {
  if (!input.consent) {
    throw new CliInputError(
      `Pass --consent to confirm: ${VOICE_CONSENT_STATEMENT}`,
    );
  }
  let size: number;
  try {
    const info = await stat(input.file);
    if (!info.isFile()) throw new Error("not a file");
    size = info.size;
  } catch {
    throw new CliInputError(`Cannot read audio file: ${input.file}`);
  }
  if (size > VOICE_MAX_UPLOAD_BYTES) {
    throw new CliInputError(
      `Audio exceeds the ${VOICE_MAX_UPLOAD_BYTES / (1024 * 1024)} MiB limit.`,
    );
  }
  const audio = new Uint8Array(await Bun.file(input.file).arrayBuffer());
  const voice = await asCliError(() =>
    store(ctx).create({
      name: input.name,
      audio,
      consent: true,
      createdBy: CLI_CREATOR,
    }),
  );
  execution.output.info(`Added voice ${describe(voice)}`);
  execution.output.info(
    `Use it with "voice": "${voice.id}" on /v1/audio/speech.`,
  );
  return { data: { voice } };
}

export async function runVoicesRemove(
  input: VoicesRemoveInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { voice: CustomVoice } }> {
  const voice = await asCliError(() => store(ctx).remove(input.voiceId));
  execution.output.info(`Removed voice ${voice.id} (${voice.name})`);
  return { data: { voice } };
}
