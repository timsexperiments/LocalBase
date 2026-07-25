import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig, saveConfig } from "../../../manager";
import type { CommandExecution } from "../../app/commands/framework";
import { promptSetInputSchema } from "../../app/commands/inputs";
import {
  effectiveSystemPrompt,
  runPromptReset,
  runPromptSet,
  runPromptShow,
} from "./prompt";

const execution: CommandExecution = {
  global: { json: false, nonInteractive: true },
  output: { info() {}, error() {}, lifecycle() {} },
};

function createContext(root: string): AppContext {
  const database = new DatabaseSession();
  const config = defaultConfig(root);
  saveConfig(database, config);
  return {
    database,
    config,
    specs: {
      osName: "Test OS",
      ramGb: 32,
      cpuModel: "Test CPU",
      gpuName: "Test GPU",
      gpuVramGb: 16,
      isMac: false,
      isAppleSilicon: false,
    },
    logger: {
      info() {},
      warn() {},
      error() {},
      request() {},
      pipeStream() {},
    },
  };
}

async function withContext(action: (ctx: AppContext) => Promise<void>) {
  const root = mkdtempSync(join(tmpdir(), "localbase-prompt-command-"));
  const ctx = createContext(root);
  try {
    await action(ctx);
  } finally {
    ctx.database.close();
    rmSync(root, { recursive: true, force: true });
  }
}

test("prompt commands manage a model override and its global fallback", async () => {
  await withContext(async (ctx) => {
    const modelId = "qwen2.5-coder-7b-instruct-q4_k_m";
    await runPromptSet({ text: ["Global", "fallback"] }, ctx, execution);
    await runPromptSet(
      { model: modelId, text: ["Model", "override"] },
      ctx,
      execution,
    );

    expect(effectiveSystemPrompt(ctx, modelId)).toEqual({
      prompt: "Model override",
      source: "model",
    });

    await expect(
      runPromptShow({ model: modelId }, ctx, execution),
    ).resolves.toMatchObject({
      data: {
        scope: "model",
        modelId,
        prompt: "Model override",
        source: "model",
      },
    });

    await expect(
      runPromptReset({ model: modelId }, ctx, execution),
    ).resolves.toMatchObject({
      data: { updated: true, scope: "model", modelId },
    });
    expect(effectiveSystemPrompt(ctx, modelId)).toEqual({
      prompt: "Global fallback",
      source: "global",
    });
  });
});

test("prompt input rejects file conflicts, invalid model kinds, and empty files", async () => {
  expect(
    promptSetInputSchema.safeParse({
      model: "whisper-base-q8_0",
      text: ["Prompt"],
    }).success,
  ).toBe(false);
  expect(
    promptSetInputSchema.safeParse({
      file: "prompt.txt",
      text: ["Prompt"],
    }).success,
  ).toBe(false);

  await withContext(async (ctx) => {
    const file = join(ctx.config.root, "empty-prompt.txt");
    await Bun.write(file, "\n \t");
    await expect(
      runPromptSet({ file, text: [] }, ctx, execution),
    ).rejects.toThrow("cannot be empty");
  });
});
