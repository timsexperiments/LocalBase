import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig } from "../../../manager";
import { toneWav } from "../../../test/voice-audio-fixture";
import { CliInputError } from "../../app/commands/errors";
import type { CommandExecution } from "../../app/commands/framework";
import { voicesListResultSchema } from "../../app/commands/results";
import { customVoicePath } from "../voice-store";
import { runVoicesAdd, runVoicesList, runVoicesRemove } from "./voices";

const directory = mkdtempSync(join(tmpdir(), "local-base-voices-cli-"));
const database = new DatabaseSession();
afterAll(() => {
  database.close();
  rmSync(directory, { recursive: true, force: true });
});

const lines: string[] = [];
const execution: CommandExecution = {
  global: { nonInteractive: true, json: false },
  output: {
    info: (line: string) => void lines.push(line),
    error() {},
    lifecycle() {},
  },
};

test("voices add, list and remove share the gateway store", async () => {
  const config = defaultConfig(join(directory, "root"));
  const ctx = { config, database } as unknown as AppContext;
  const file = join(directory, "clip.wav");
  writeFileSync(file, toneWav({ seconds: 3.2 }));

  await expect(
    runVoicesAdd({ file, name: "Narrator", consent: false }, ctx, execution),
  ).rejects.toThrow("Pass --consent");
  await expect(
    runVoicesAdd({ file, name: "harbor", consent: true }, ctx, execution),
  ).rejects.toBeInstanceOf(CliInputError);
  await expect(
    runVoicesAdd(
      { file: join(directory, "missing.wav"), name: "N", consent: true },
      ctx,
      execution,
    ),
  ).rejects.toThrow("Cannot read audio file");

  const { voice } = (
    await runVoicesAdd(
      { file, name: "Narrator", consent: true },
      ctx,
      execution,
    )
  ).data;
  expect(voice).toMatchObject({ name: "Narrator", createdBy: "cli" });
  expect(await Bun.file(customVoicePath(config.root, voice.id)).exists()).toBe(
    true,
  );
  const listed = runVoicesList({}, ctx, execution).data;
  expect(voicesListResultSchema.parse(listed).voices).toEqual([voice]);
  expect(lines.some((line) => line.includes(voice.id))).toBe(true);

  await expect(
    runVoicesRemove({ voiceId: "../local-base.db" }, ctx, execution),
  ).rejects.toThrow("Voice not found.");
  expect(
    (await runVoicesRemove({ voiceId: voice.id }, ctx, execution)).data,
  ).toEqual({ voice });
  expect(await Bun.file(customVoicePath(config.root, voice.id)).exists()).toBe(
    false,
  );
  expect(runVoicesList({}, ctx, execution).data.voices).toEqual([]);
});
