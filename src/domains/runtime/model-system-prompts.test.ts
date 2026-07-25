import { expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { migrate } from "drizzle-orm/bun-sqlite/migrator";
import { migrationsFolder } from "../../db/migration-assets";
import * as schema from "../../db/schema";
import {
  deleteModelSystemPrompt,
  getModelSystemPrompt,
  saveModelSystemPrompt,
} from "./model-system-prompts";

function createDatabase() {
  const sqlite = new Database(":memory:");
  const database = drizzle({ client: sqlite, schema });
  migrate(database, { migrationsFolder: migrationsFolder() });
  return { database, sqlite };
}

test("persists, replaces, reads, and deletes model system prompts", () => {
  const { database, sqlite } = createDatabase();
  const modelId = "qwen2.5-coder-7b-instruct-q4_k_m";
  try {
    saveModelSystemPrompt(database, { modelId, prompt: "Be concise." });
    expect(getModelSystemPrompt(database, modelId)).toEqual({
      modelId,
      prompt: "Be concise.",
    });

    saveModelSystemPrompt(database, { modelId, prompt: "Explain tradeoffs." });
    expect(getModelSystemPrompt(database, modelId)).toEqual({
      modelId,
      prompt: "Explain tradeoffs.",
    });

    deleteModelSystemPrompt(database, modelId);
    expect(getModelSystemPrompt(database, modelId)).toBeUndefined();
  } finally {
    sqlite.close();
  }
});

test("rejects non-LLM inputs and malformed persisted prompt rows", () => {
  const { database, sqlite } = createDatabase();
  const modelId = "qwen2.5-coder-7b-instruct-q4_k_m";
  try {
    expect(() =>
      saveModelSystemPrompt(database, {
        modelId: "whisper-base-q8_0",
        prompt: "Invalid model kind.",
      }),
    ).toThrow("catalog LLM");
    expect(() =>
      saveModelSystemPrompt(database, { modelId, prompt: "   " }),
    ).toThrow("cannot be empty");

    database
      .insert(schema.modelSystemPromptsTable)
      .values({ modelId, prompt: "   " })
      .run();
    expect(() => getModelSystemPrompt(database, modelId)).toThrow(
      "cannot be empty",
    );
  } finally {
    sqlite.close();
  }
});
