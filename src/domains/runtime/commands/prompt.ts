import { type AppContext } from "../../../context";
import { saveConfig } from "../../../manager";
import type { CommandExecution } from "../../app/commands/framework";
import { CliInputError } from "../../app/commands/errors";
import type {
  PromptResetInput,
  PromptSetInput,
  PromptShowInput,
} from "../../app/commands/inputs";

export const DEFAULT_SYSTEM_PROMPT = `You are an expert AI software engineer and system architect. You provide helpful, correct, and highly optimized code implementations.
Guidelines:
- Design: Think step-by-step. Break down your reasoning before writing code.
- Quality: Write clean, production-grade, complete code blocks. Never omit parts or use temporary placeholders like "// TODO" or "// implement later".
- Explanation: Keep explanations concise and focused on the "why" and non-obvious details rather than repeating what the code does.
- Formatting: Always format output in clear Markdown with appropriate syntax highlighting.
- Output Policy: Respond directly in plain Markdown. Never start or wrap your responses with XML/HTML tags like <system-reminder>, unless explicitly instructed to do so.`;

export async function runPromptShow(
  _input: PromptShowInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { prompt: string; source: "custom" | "default" } }> {
  const config = ctx.config;
  const prompt = config.systemPrompt || DEFAULT_SYSTEM_PROMPT;
  const isCustom = !!config.systemPrompt;

  execution.output.info(
    `\nActive System Prompt (${isCustom ? "Custom" : "Default fallback"}):`,
  );
  execution.output.info(
    "--------------------------------------------------------------------------------",
  );
  execution.output.info(prompt);
  execution.output.info(
    "--------------------------------------------------------------------------------",
  );
  return { data: { prompt, source: isCustom ? "custom" : "default" } };
}

export async function runPromptSet(
  input: PromptSetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { updated: true; configured: boolean } }> {
  const config = ctx.config;
  let promptText = "";

  const file = input.file;
  if (file) {
    const promptFile = Bun.file(file);
    if (!(await promptFile.exists())) {
      throw new CliInputError(`File not found at "${file}"`);
    }
    promptText = (await promptFile.text()).trim();
  } else {
    if (input.text.length > 0) {
      promptText = input.text.join(" ").trim();
    } else {
      // Read from stdin if not a TTY
      if (!process.stdin.isTTY) {
        promptText = await readStdin();
      } else {
        throw new CliInputError(
          "Provide prompt text, specify --file <path>, or pipe text to stdin.",
        );
      }
    }
  }

  if (!promptText) {
    throw new CliInputError("Custom system prompt cannot be empty.");
  }

  config.systemPrompt = promptText;
  saveConfig(ctx.database, config);
  execution.output.info("\n✅ Custom system prompt updated successfully.");
  return { data: { updated: true, configured: true } };
}

export async function runPromptReset(
  _input: PromptResetInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{ data: { updated: true; configured: boolean } }> {
  const config = ctx.config;
  config.systemPrompt = "";
  saveConfig(ctx.database, config);
  execution.output.info(
    "\n✅ Custom system prompt reset back to default assistant persona.",
  );
  return { data: { updated: true, configured: false } };
}

async function readStdin(): Promise<string> {
  const chunks: string[] = [];
  for await (const chunk of Bun.stdin.stream()) {
    chunks.push(new TextDecoder().decode(chunk));
  }
  return chunks.join("");
}
