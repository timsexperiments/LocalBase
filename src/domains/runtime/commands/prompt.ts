import { type AppContext } from "../../../context";
import { withDatabase } from "../../../db/client";
import { saveConfig } from "../../../manager";
import type { CommandExecution } from "../../app/commands/framework";
import { CliInputError } from "../../app/commands/errors";
import type {
  PromptResetInput,
  PromptSetInput,
  PromptShowInput,
} from "../../app/commands/inputs";
import {
  deleteModelSystemPrompt,
  getModelSystemPrompt,
  saveModelSystemPrompt,
  systemPromptTextSchema,
} from "../model-system-prompts";

export const DEFAULT_SYSTEM_PROMPT = `You are an expert AI software engineer and system architect. You provide helpful, correct, and highly optimized code implementations.
Guidelines:
- Design: Think step-by-step. Break down your reasoning before writing code.
- Quality: Write clean, production-grade, complete code blocks. Never omit parts or use temporary placeholders like "// TODO" or "// implement later".
- Explanation: Keep explanations concise and focused on the "why" and non-obvious details rather than repeating what the code does.
- Formatting: Always format output in clear Markdown with appropriate syntax highlighting.
- Output Policy: Respond directly in plain Markdown. Never start or wrap your responses with XML/HTML tags like <system-reminder>, unless explicitly instructed to do so.`;

export type EffectiveSystemPrompt = {
  prompt: string;
  source: "model" | "global" | "built-in";
};

type PromptScope = { scope: "global" } | { scope: "model"; modelId: string };

export function effectiveSystemPrompt(
  ctx: AppContext,
  modelId?: string,
): EffectiveSystemPrompt {
  const override = modelId
    ? withDatabase(ctx.database, ctx.config.root, (database) =>
        getModelSystemPrompt(database, modelId),
      )
    : undefined;
  if (override) return { prompt: override.prompt, source: "model" };
  if (ctx.config.systemPrompt) {
    return { prompt: ctx.config.systemPrompt, source: "global" };
  }
  return { prompt: DEFAULT_SYSTEM_PROMPT, source: "built-in" };
}

function promptSourceLabel(source: EffectiveSystemPrompt["source"]): string {
  if (source === "model") return "Model override";
  if (source === "global") return "Global fallback";
  return "Built-in default";
}

export async function runPromptShow(
  input: PromptShowInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: PromptScope & EffectiveSystemPrompt }> {
  const { prompt, source } = effectiveSystemPrompt(ctx, input.model);
  const scope = promptScope(input.model);

  execution.output.info(
    `\nEffective System Prompt (${promptSourceLabel(source)}):`,
  );
  execution.output.info(
    "--------------------------------------------------------------------------------",
  );
  execution.output.info(prompt);
  execution.output.info(
    "--------------------------------------------------------------------------------",
  );
  return { data: { ...scope, prompt, source } };
}

export async function runPromptSet(
  input: PromptSetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: PromptScope & { updated: true } }> {
  const promptText = await resolvePromptText(input);

  if (input.model) {
    withDatabase(ctx.database, ctx.config.root, (database) => {
      saveModelSystemPrompt(database, {
        modelId: input.model!,
        prompt: promptText,
      });
    });
    execution.output.info("\n✅ Model system prompt updated successfully.");
    return { data: { updated: true, scope: "model", modelId: input.model } };
  }

  ctx.config.systemPrompt = promptText;
  saveConfig(ctx.database, ctx.config);
  execution.output.info("\n✅ Global system prompt updated successfully.");
  return { data: { updated: true, scope: "global" } };
}

export async function runPromptReset(
  input: PromptResetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: PromptScope & { updated: true } }> {
  if (input.model) {
    withDatabase(ctx.database, ctx.config.root, (database) => {
      deleteModelSystemPrompt(database, input.model!);
    });
    execution.output.info("\n✅ Model system prompt reset to its fallback.");
    return { data: { updated: true, scope: "model", modelId: input.model } };
  }

  ctx.config.systemPrompt = "";
  saveConfig(ctx.database, ctx.config);
  execution.output.info(
    "\n✅ Global system prompt reset to the built-in default.",
  );
  return { data: { updated: true, scope: "global" } };
}

function promptScope(modelId: string | undefined): PromptScope {
  return modelId ? { scope: "model", modelId } : { scope: "global" };
}

async function resolvePromptText(input: PromptSetInput): Promise<string> {
  let text: string;
  if (input.file) {
    const promptFile = Bun.file(input.file);
    if (!(await promptFile.exists())) {
      throw new CliInputError(`File not found at "${input.file}"`);
    }
    text = await promptFile.text();
  } else if (input.text.length > 0) {
    text = input.text.join(" ");
  } else if (!process.stdin.isTTY) {
    text = await readStdin();
  } else {
    throw new CliInputError(
      "Provide prompt text, specify --file <path>, or pipe text to stdin.",
    );
  }

  const parsed = systemPromptTextSchema.safeParse(text.trim());
  if (!parsed.success) {
    throw new CliInputError("Custom system prompt cannot be empty.");
  }
  return parsed.data;
}

async function readStdin(): Promise<string> {
  const chunks: string[] = [];
  for await (const chunk of Bun.stdin.stream()) {
    chunks.push(new TextDecoder().decode(chunk));
  }
  return chunks.join("");
}
