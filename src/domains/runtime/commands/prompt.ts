import { type AppContext } from "../../../context";
import { parseFlag } from "../../../utils/args";
import { saveConfig } from "../../../manager";

export const DEFAULT_SYSTEM_PROMPT = `You are an expert AI software engineer and system architect. You provide helpful, correct, and highly optimized code implementations.
Guidelines:
- Design: Think step-by-step. Break down your reasoning before writing code.
- Quality: Write clean, production-grade, complete code blocks. Never omit parts or use temporary placeholders like "// TODO" or "// implement later".
- Explanation: Keep explanations concise and focused on the "why" and non-obvious details rather than repeating what the code does.
- Formatting: Always format output in clear Markdown with appropriate syntax highlighting.
- Output Policy: Respond directly in plain Markdown. Never start or wrap your responses with XML/HTML tags like <system-reminder>, unless explicitly instructed to do so.`;

export async function runPromptShow(ctx: AppContext): Promise<number> {
  const config = ctx.config;
  const prompt = config.systemPrompt || DEFAULT_SYSTEM_PROMPT;
  const isCustom = !!config.systemPrompt;

  console.log(
    `\nActive System Prompt (${isCustom ? "Custom" : "Default fallback"}):`,
  );
  console.log(
    "--------------------------------------------------------------------------------",
  );
  console.log(prompt);
  console.log(
    "--------------------------------------------------------------------------------",
  );
  return 0;
}

export async function runPromptSet(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const config = ctx.config;
  let promptText = "";

  const file = parseFlag(args, "--file");
  if (file) {
    const promptFile = Bun.file(file);
    if (!(await promptFile.exists())) {
      console.error(`Error: File not found at "${file}"`);
      return 1;
    }
    promptText = (await promptFile.text()).trim();
  } else {
    // Collect all positional arguments after 'prompt' and 'set'
    const promptIdx = args.indexOf("prompt");
    const setIdx = args.indexOf("set");
    const startIdx = Math.max(promptIdx + 1, setIdx + 1);

    const restArgs =
      startIdx > 0
        ? args.slice(startIdx).filter((a) => !a.startsWith("--"))
        : [];

    if (restArgs.length > 0) {
      promptText = restArgs.join(" ").trim();
    } else {
      // Read from stdin if not a TTY
      if (!process.stdin.isTTY) {
        promptText = await readStdin();
      } else {
        console.error(
          "Error: Please provide a prompt string, specify --file <path>, or pipe to stdin.",
        );
        console.error(
          'Usage: local-base prompt set "Your custom instructions"',
        );
        console.error("       local-base prompt set --file path/to/prompt.txt");
        return 1;
      }
    }
  }

  if (!promptText) {
    console.error("Error: Custom system prompt cannot be empty.");
    return 1;
  }

  config.systemPrompt = promptText;
  saveConfig(ctx.database, config);
  console.log("\n✅ Custom system prompt updated successfully.");
  return 0;
}

export async function runPromptReset(ctx: AppContext): Promise<number> {
  const config = ctx.config;
  config.systemPrompt = "";
  saveConfig(ctx.database, config);
  console.log(
    "\n✅ Custom system prompt reset back to default assistant persona.",
  );
  return 0;
}

async function readStdin(): Promise<string> {
  const chunks: string[] = [];
  for await (const chunk of Bun.stdin.stream()) {
    chunks.push(new TextDecoder().decode(chunk));
  }
  return chunks.join("");
}
