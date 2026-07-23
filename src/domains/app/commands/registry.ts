import type { CLICommand } from "./types";

const configureFlags: NonNullable<CLICommand["flags"]> = [
  {
    name: "--defaults",
    type: "boolean",
    description: "Use saved or default settings without prompting",
  },
  {
    name: "--all",
    type: "boolean",
    description: "Prompt for every configurable setting",
  },
  {
    name: "--config",
    type: "file.toml",
    description: "Load configuration overrides from a TOML file",
  },
  { name: "--root", type: "path", description: "Storage root directory path" },
  { name: "--host", type: "host", description: "LLM binding host address" },
  { name: "--port", type: "n", description: "LLM binding port number" },
  {
    name: "--ctx-size",
    type: "n",
    description: "LLM maximum context limit ceiling",
  },
  {
    name: "--parallel",
    type: "n|auto",
    description: "Parallel slots: auto or an integer from 1 to 4",
  },
  { name: "--stt-host", type: "host", description: "STT binding host address" },
  { name: "--stt-port", type: "n", description: "STT binding port number" },
  {
    name: "--startup-on-boot",
    type: "true|false",
    description: "Configure system service startup on boot",
  },
  {
    name: "--llm-models",
    type: "id1,id2",
    description: "Comma-separated LLM model IDs",
  },
  {
    name: "--stt-models",
    type: "id1,id2",
    description: "Comma-separated STT model IDs; empty disables STT",
  },
  {
    name: "--image-models",
    type: "id1,id2",
    description:
      "Comma-separated image model IDs; empty disables image generation",
  },
  { name: "--active-llm", type: "id", description: "Active LLM model ID" },
  { name: "--active-stt", type: "id", description: "Active STT model ID" },
  { name: "--active-image", type: "id", description: "Active image model ID" },
  {
    name: "--hf-token",
    type: "token",
    description: "Hugging Face token for gated model downloads",
  },
  {
    name: "--create-key",
    type: "true|false",
    description: "Create an initial API key during configuration",
  },
];

export const commandRegistry: CLICommand[] = [
  {
    name: "",
    description: "Default action: interactive configuration setup",
    handler: async (args, ctx) => {
      const { runConfigure } = await import("../../config/commands/configure");
      return runConfigure(args, ctx);
    },
    flags: configureFlags,
  },
  {
    name: "init",
    description: "Initialize the database and directories in the storage root",
    handler: async (args, ctx) => {
      const { runInit } = await import("../../config/commands/init");
      return runInit(args, ctx);
    },
    flags: [
      {
        name: "--root",
        type: "path",
        description: "Storage root directory path",
      },
    ],
  },
  {
    name: "configure",
    description: "Configure models, ports, settings, and create API keys",
    handler: async (args, ctx) => {
      const { runConfigure } = await import("../../config/commands/configure");
      return runConfigure(args, ctx);
    },
    flags: configureFlags,
  },
  {
    name: "doctor",
    description: "Run system health check and print configuration details",
    handler: async (args, ctx) => {
      const { runDoctor } = await import("../../system/commands/doctor");
      return runDoctor(args, ctx);
    },
    flags: [
      {
        name: "--json",
        type: "boolean",
        description: "Output system information as JSON",
      },
    ],
  },
  {
    name: "catalog",
    description: "List all supported models in the model registry catalog",
    handler: async (args, ctx) => {
      const { runCatalog } = await import("../../models/commands/catalog");
      return runCatalog(args, ctx);
    },
    flags: [
      {
        name: "--kind",
        type: "llm|stt|image",
        description: "Filter models by kind",
      },
    ],
  },
  {
    name: "recommend",
    description:
      "Recommend models based on system specifications and VRAM limit",
    handler: async (args, ctx) => {
      const { runRecommend } = await import("../../models/commands/recommend");
      return runRecommend(args, ctx);
    },
    flags: [
      {
        name: "--kind",
        type: "llm|stt|image",
        description: "Filter recommendations by kind",
      },
      {
        name: "--vram",
        type: "gb",
        description: "Specify target VRAM in GB for memory check calculations",
      },
    ],
  },
  {
    name: "installed",
    description: "List models currently installed in the storage root",
    handler: async (args, ctx) => {
      const { runInstalled } = await import("../../models/commands/installed");
      return runInstalled(args, ctx);
    },
    flags: [
      {
        name: "--kind",
        type: "llm|stt|image",
        description: "Filter listed models by kind",
      },
    ],
  },

  {
    name: "install",
    description: "Download and install a model from repository by ID",
    handler: async (args, ctx) => {
      const { runInstall } = await import("../../models/commands/install");
      return runInstall(args, ctx);
    },
    positional: ["[model_id]"],
    flags: [
      {
        name: "--all",
        type: "boolean",
        description: "Download and install all selected models",
      },
    ],
  },
  {
    name: "serve",
    description: "Start the unified LocalBase API gateway and LLM/STT backends",
    handler: async (args, ctx) => {
      const { runServe } = await import("../../runtime/commands/serve");
      return runServe(args, ctx);
    },
    flags: [
      {
        name: "--root",
        type: "path",
        description: "Storage root directory path",
      },
      { name: "--host", type: "host", description: "API gateway binding host" },
      {
        name: "--port",
        type: "port",
        description: "API gateway binding port (defaults to 2273)",
      },
      {
        name: "--llm",
        type: "true|false",
        description: "Enable/disable the LLM service (defaults to true)",
      },
      {
        name: "--stt",
        type: "true|false",
        description: "Enable/disable the STT service (defaults to true)",
      },
      {
        name: "--llm-host",
        type: "host",
        description: "Host for the upstream llama-server",
      },
      {
        name: "--llm-port",
        type: "port",
        description: "Port for the upstream llama-server",
      },
      {
        name: "--stt-host",
        type: "host",
        description: "Host for the upstream whisper-server",
      },
      {
        name: "--stt-port",
        type: "port",
        description: "Port for the upstream whisper-server",
      },
      {
        name: "--ctx-size",
        type: "tokens",
        description: "Explicit override for LLM context size limit",
      },
      {
        name: "--stt-path",
        type: "path",
        description:
          "Explicit path override for Whisper transcription endpoint (defaults to preserving incoming path)",
      },
      {
        name: "--llm-model-file",
        type: "file",
        description: "Filename override for the active LLM GGUF model",
      },
      {
        name: "--stt-model-file",
        type: "file",
        description: "Filename override for the active STT model",
      },
      {
        name: "--image-host",
        type: "host",
        description: "Host for the upstream image-generation server",
      },
      {
        name: "--image-port",
        type: "port",
        description: "Port for the upstream image-generation server",
      },
      {
        name: "--image",
        type: "true|false",
        description: "Enable or disable image generation",
      },
      {
        name: "--image-model-file",
        type: "file",
        description: "Filename override for the active image model",
      },
      {
        name: "--auth",
        type: "true|false",
        description: "Enable/disable API key authentication check",
      },
      {
        name: "--auth-mode",
        type: "bearer|x-api-key|either",
        description: "Authentication header mode to enforce",
      },
      {
        name: "--bypass-memory-check",
        type: "boolean",
        description: "Start services without enforcing model memory checks",
      },
      {
        name: "--force",
        type: "boolean",
        description: "Alias for bypassing model memory checks",
      },
    ],
  },
  {
    name: "prompt show",
    description: "Display the active system prompt used for LLM completions",
    handler: async (_, ctx) => {
      const { runPromptShow } = await import("../../runtime/commands/prompt");
      return runPromptShow(ctx);
    },
  },
  {
    name: "prompt set",
    description:
      "Set a custom system prompt (accepts text, --file <path>, or stdin)",
    handler: async (args, ctx) => {
      const { runPromptSet } = await import("../../runtime/commands/prompt");
      return runPromptSet(args, ctx);
    },
    positional: ["[text...]"],
    flags: [
      {
        name: "--file",
        type: "path",
        description: "Path to a text file containing the system prompt",
      },
    ],
  },
  {
    name: "prompt reset",
    description:
      "Reset the custom system prompt back to the default assistant persona",
    handler: async (_, ctx) => {
      const { runPromptReset } = await import("../../runtime/commands/prompt");
      return runPromptReset(ctx);
    },
  },
  {
    name: "keys list",
    description: "List all active API keys and their usage stats",
    handler: async (args, ctx) => {
      const { runKeys } = await import("../../auth/commands/keys");
      return runKeys(args, ctx);
    },
  },
  {
    name: "keys create",
    description: "Create a new API key for client authentication",
    handler: async (args, ctx) => {
      const { runKeys } = await import("../../auth/commands/keys");
      return runKeys(args, ctx);
    },
    flags: [
      {
        name: "--name",
        type: "label",
        description: "Descriptive label for the API key",
      },
      {
        name: "--expires-days",
        type: "n",
        description: "Optional expiration period in days",
      },
    ],
  },
  {
    name: "keys revoke",
    description: "Revoke/deactivate an API key by ID",
    handler: async (args, ctx) => {
      const { runKeys } = await import("../../auth/commands/keys");
      return runKeys(args, ctx);
    },
    positional: ["<key_id>"],
  },
  {
    name: "keys rotate",
    description: "Rotate/replace an API key with a new secret key",
    handler: async (args, ctx) => {
      const { runKeys } = await import("../../auth/commands/keys");
      return runKeys(args, ctx);
    },
    positional: ["<key_id>"],
  },
  {
    name: "reset",
    description:
      "Delete the configuration database and recreate default settings",
    requiresDatabase: false,
    handler: async (args, ctx) => {
      const { runReset } = await import("../../maintenance/commands/reset");
      return runReset(args, ctx);
    },
    flags: [
      {
        name: "--root",
        type: "path",
        description: "Storage root path to reset",
      },
      {
        name: "--yes",
        type: "boolean",
        short: "y",
        description: "Confirm reset without interactive prompting (also -y)",
      },
    ],
  },
  {
    name: "uninstall",
    description:
      "Remove all LocalBase-managed data under the selected storage root",
    requiresDatabase: false,
    handler: async (args, ctx) => {
      const { runUninstall } =
        await import("../../maintenance/commands/uninstall");
      return runUninstall(args, ctx);
    },
    flags: [
      {
        name: "--root",
        type: "path",
        description: "Storage root path to uninstall",
      },
      {
        name: "--yes",
        type: "boolean",
        short: "y",
        description:
          "Confirm uninstallation without interactive prompting (also -y)",
      },
    ],
  },
];
