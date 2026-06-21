import type { AppContext } from "../../../context";
import { runInit } from "../../config/commands/init";
import { runConfigure } from "../../config/commands/configure";
import { runDoctor } from "../../system/commands/doctor";
import { runCatalog } from "../../models/commands/catalog";
import { runRecommend } from "../../models/commands/recommend";
import { runInstalled } from "../../models/commands/installed";
import { runInstall } from "../../models/commands/install";
import { runServe } from "../../runtime/commands/serve";
import { runKeys } from "../../auth/commands/keys";
import { runReset } from "../../maintenance/commands/reset";
import { runUninstall } from "../../maintenance/commands/uninstall";

interface CommandFlag {
  name: string;
  type: string;
  description: string;
}

export interface CLICommand {
  name: string;
  description: string;
  positional?: string[];
  flags?: CommandFlag[];
  handler: (args: string[], ctx: AppContext) => Promise<number> | number;
}

export const commandRegistry: CLICommand[] = [
  {
    name: "",
    description: "Default action: interactive configuration setup",
    handler: runConfigure,
    flags: [
      { name: "--defaults", type: "boolean", description: "Use default settings without prompting" },
      { name: "--all", type: "boolean", description: "Prompt for all available settings interactively" },
      { name: "--config", type: "file.toml", description: "Load configuration overrides from a TOML file" },
      { name: "--root", type: "path", description: "Storage root directory path" },
      { name: "--host", type: "host", description: "LLM binding host address" },
      { name: "--port", type: "n", description: "LLM binding port number" },
      { name: "--ctx-size", type: "n", description: "LLM maximum context limit ceiling" },
      { name: "--stt-host", type: "host", description: "STT binding host address" },
      { name: "--stt-port", type: "n", description: "STT binding port number" },
      { name: "--startup-on-boot", type: "true|false", description: "Configure system service startup on boot" },
      { name: "--llm-models", type: "id1,id2", description: "Comma-separated list of LLM models to select" },
      { name: "--stt-models", type: "id1,id2", description: "Comma-separated list of STT models to select" },
      { name: "--active-llm", type: "id", description: "Sets the active LLM model ID" },
      { name: "--active-stt", type: "id", description: "Sets the active STT model ID" },
      { name: "--create-key", type: "true|false", description: "Create an initial API key during configuration" }
    ]
  },
  {
    name: "init",
    description: "Initialize the database and directories in the storage root",
    handler: runInit,
    flags: [
      { name: "--root", type: "path", description: "Storage root directory path" }
    ]
  },
  {
    name: "configure",
    description: "Configure models, ports, settings, and create API keys",
    handler: runConfigure,
    flags: [
      { name: "--defaults", type: "boolean", description: "Use default settings without prompting" },
      { name: "--all", type: "boolean", description: "Interactive prompt for all settings" },
      { name: "--config", type: "file.toml", description: "Load configuration overrides from a TOML file" },
      { name: "--root", type: "path", description: "Storage root directory path" },
      { name: "--host", type: "host", description: "LLM host address" },
      { name: "--port", type: "n", description: "LLM port number" },
      { name: "--ctx-size", type: "n", description: "LLM maximum context limit ceiling" },
      { name: "--stt-host", type: "host", description: "STT host address" },
      { name: "--stt-port", type: "n", description: "STT port number" },
      { name: "--startup-on-boot", type: "true|false", description: "Configure system service startup on boot" },
      { name: "--llm-models", type: "id1,id2", description: "Comma-separated list of LLM models to select" },
      { name: "--stt-models", type: "id1,id2", description: "Comma-separated list of STT models to select" },
      { name: "--active-llm", type: "id", description: "Sets the active LLM model ID" },
      { name: "--active-stt", type: "id", description: "Sets the active STT model ID" },
      { name: "--create-key", type: "true|false", description: "Create an initial API key during configuration" }
    ]
  },
  {
    name: "doctor",
    description: "Run system health check and print configuration details",
    handler: runDoctor,
    flags: [
      { name: "--json", type: "boolean", description: "Output system information as JSON" }
    ]
  },
  {
    name: "catalog",
    description: "List all supported models in the model registry catalog",
    handler: runCatalog,
    flags: [
      { name: "--kind", type: "llm|stt|tts|image|video|audio", description: "Filter models by kind" }
    ]
  },
  {
    name: "recommend",
    description: "Recommend models based on system specifications and VRAM limit",
    handler: runRecommend,
    flags: [
      { name: "--kind", type: "llm|stt|tts|image|video|audio", description: "Filter recommendations by kind" },
      { name: "--vram", type: "gb", description: "Specify target VRAM in GB for memory check calculations" }
    ]
  },
  {
    name: "installed",
    description: "List models currently installed in the storage root",
    handler: runInstalled,
    flags: [
      { name: "--kind", type: "llm|stt|tts|image|video|audio", description: "Filter listed models by kind" }
    ]
  },
  {
    name: "install",
    description: "Download and install a model from repository by ID",
    handler: runInstall,
    positional: ["<model_id>"]
  },
  {
    name: "serve",
    description: "Start the unified LocalBase API gateway and LLM/STT backends",
    handler: runServe,
    flags: [
      { name: "--host", type: "host", description: "API gateway binding host" },
      { name: "--port", type: "port", description: "API gateway binding port (defaults to 8787)" },
      { name: "--llm", type: "true|false", description: "Enable/disable the LLM service (defaults to true)" },
      { name: "--stt", type: "true|false", description: "Enable/disable the STT service (defaults to true)" },
      { name: "--tts", type: "true|false", description: "Enable/disable TTS routing" },
      { name: "--image", type: "true|false", description: "Enable/disable Image generation routing" },
      { name: "--video", type: "true|false", description: "Enable/disable Video generation routing" },
      { name: "--llm-host", type: "host", description: "Host for the upstream llama-server" },
      { name: "--llm-port", type: "port", description: "Port for the upstream llama-server" },
      { name: "--stt-host", type: "host", description: "Host for the upstream whisper-server" },
      { name: "--stt-port", type: "port", description: "Port for the upstream whisper-server" },
      { name: "--ctx-size", type: "tokens", description: "Explicit override for LLM context size limit" },
      { name: "--stt-path", type: "path", description: "Path for Whisper transcription proxy endpoint" },
      { name: "--llm-model-file", type: "file", description: "Filename override for the active LLM GGUF model" },
      { name: "--stt-model-file", type: "file", description: "Filename override for the active STT model" },
      { name: "--auth", type: "true|false", description: "Enable/disable API key authentication check" },
      { name: "--auth-mode", type: "bearer|x-api-key|either", description: "Authentication header mode to enforce" },
      { name: "--tts-upstream", type: "url", description: "Upstream URL for speech generation endpoint" },
      { name: "--image-upstream", type: "url", description: "Upstream URL for image generation endpoint" },
      { name: "--video-upstream", type: "url", description: "Upstream URL for video generation endpoint" }
    ]
  },
  {
    name: "keys list",
    description: "List all active API keys and their usage stats",
    handler: runKeys
  },
  {
    name: "keys create",
    description: "Create a new API key for client authentication",
    handler: runKeys,
    flags: [
      { name: "--name", type: "label", description: "Descriptive label for the API key" },
      { name: "--expires-days", type: "n", description: "Optional expiration period in days" }
    ]
  },
  {
    name: "keys revoke",
    description: "Revoke/deactivate an API key by ID",
    handler: runKeys,
    positional: ["<key_id>"]
  },
  {
    name: "keys rotate",
    description: "Rotate/replace an API key with a new secret key",
    handler: runKeys,
    positional: ["<key_id>"]
  },
  {
    name: "reset",
    description: "Reset database and purge selected configuration state",
    handler: runReset,
    flags: [
      { name: "--root", type: "path", description: "Storage root path to reset" },
      { name: "--yes", type: "boolean", description: "Confirm reset without interactive prompting" }
    ]
  },
  {
    name: "uninstall",
    description: "Remove installed binaries, models, database and cleanup system settings",
    handler: runUninstall,
    flags: [
      { name: "--root", type: "path", description: "Storage root path to uninstall" },
      { name: "--yes", type: "boolean", description: "Confirm uninstallation without interactive prompting" }
    ]
  }
];

function buildUsageLine(cmd: CLICommand): string {
  const parts = ["local-base"];
  if (cmd.name) {
    parts.push(cmd.name);
  }
  if (cmd.positional) {
    parts.push(...cmd.positional);
  }
  if (cmd.flags) {
    for (const flag of cmd.flags) {
      if (flag.type === "boolean") {
        parts.push(`[${flag.name}]`);
      } else {
        parts.push(`[${flag.name} <${flag.type}>]`);
      }
    }
  }
  return parts.join(" ");
}

export function printHelp(): void {
  console.log("local-base - Bun TypeScript local AI installer/manager\n");
  console.log("Usage:");
  
  for (const cmd of commandRegistry) {
    console.log(`  ${buildUsageLine(cmd)}`);
  }
  
  console.log("\nCommands:");
  const activeCommands = commandRegistry.filter(cmd => cmd.name !== "");
  const maxLen = Math.max(...activeCommands.map(cmd => cmd.name.length));
  
  for (const cmd of activeCommands) {
    const padded = cmd.name.padEnd(maxLen + 2, " ");
    console.log(`  ${padded}${cmd.description}`);
  }
  
  console.log("");
}
