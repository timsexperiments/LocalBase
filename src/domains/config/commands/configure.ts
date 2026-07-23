import { join } from "node:path";
import { homedir } from "node:os";
import {
  byId,
  listModels,
  evaluateModelFit,
  calculateMaxSafeContextSize,
} from "../../../catalog";
import {
  createApiKey,
  defaultRoot,
  loadApiKeys,
  loadConfig,
  saveConfig,
  type LocalBaseConfig,
} from "../../../manager";
import type { AppContext } from "../../../context";
import { detectSpecs } from "../../../system";
import { validateModelList } from "../../models/model-selection";
import { parseBool, parseFlag, parseList, toInt } from "../../../utils/args";
import {
  confirmPrompt,
  multiSelectPrompt,
  numberPrompt,
  singleSelectPrompt,
  textPrompt,
} from "../../../utils/prompt";
import { loadTomlOverrides } from "../../../utils/toml";
import { parseParallelSlots } from "../parallel";
import { z } from "zod";

export const PARALLEL_SLOTS_PROMPT =
  "Parallel request slots count (type 'auto' for dynamic auto-allocation, or an integer like 1, 2, 4)";

const continueConfigSchema = z
  .object({
    models: z.array(z.unknown()).optional(),
    tabAutocompleteModel: z.unknown().optional(),
    embeddingsProvider: z.unknown().optional(),
  })
  .passthrough();

type ConfigureFlags = {
  all: boolean;
  defaults: boolean;
  configPath?: string;
  root?: string;
  host?: string;
  port?: string;
  ctxSize?: string;
  parallel?: string;
  sttHost?: string;
  sttPort?: string;
  startupOnBoot?: string;
  llmModels?: string;
  sttModels?: string;
  imageModels?: string;
  activeLlm?: string;
  activeStt?: string;
  activeImage?: string;
  hfToken?: string;
  createKey?: string;
};

function parseConfigureFlags(args: string[]): ConfigureFlags {
  return {
    all: args.includes("--all"),
    defaults: args.includes("--defaults"),
    configPath: parseFlag(args, "--config"),
    root: parseFlag(args, "--root"),
    host: parseFlag(args, "--host"),
    port: parseFlag(args, "--port"),
    ctxSize: parseFlag(args, "--ctx-size"),
    parallel: parseFlag(args, "--parallel"),
    sttHost: parseFlag(args, "--stt-host"),
    sttPort: parseFlag(args, "--stt-port"),
    startupOnBoot: parseFlag(args, "--startup-on-boot"),
    llmModels: parseFlag(args, "--llm-models"),
    sttModels: parseFlag(args, "--stt-models"),
    imageModels: parseFlag(args, "--image-models"),
    activeLlm: parseFlag(args, "--active-llm"),
    activeStt: parseFlag(args, "--active-stt"),
    activeImage: parseFlag(args, "--active-image"),
    hfToken: parseFlag(args, "--hf-token"),
    createKey: parseFlag(args, "--create-key"),
  };
}

function continueField(value: unknown, field: string): string {
  if (!value || typeof value !== "object" || Array.isArray(value)) return "";
  const candidate = (value as Record<string, unknown>)[field];
  return typeof candidate === "string" ? candidate : "";
}

function warnAboutParallelOomRisk(
  parallel: LocalBaseConfig["parallel"],
  vramGb: number,
): void {
  if (typeof parallel === "number" && parallel > 1 && vramGb < 16) {
    console.warn(
      `Warning: Setting parallel slots to ${parallel} on a system with only ${vramGb} GB VRAM may cause Out-Of-Memory (OOM) crashes.`,
    );
  }
}

function llmChoices(
  current: string[],
  vramGb: number,
): Array<{
  name: string;
  value: string;
  checked?: boolean;
  disabled?: string | boolean;
}> {
  return listModels("llm").map((model) => {
    const fit = evaluateModelFit(model, vramGb);
    let label = `${model.modelId} (${model.storageGb.toFixed(2)}GB, min VRAM ${model.minVramGb}GB, coding ${model.codingScore}/10)`;

    let disabled: string | boolean = false;
    if (fit.status === "insufficient") {
      label += ` [❌ Requires ${model.minVramGb}GB, you have ${vramGb}GB]`;
      disabled = `Requires ${model.minVramGb}GB VRAM`;
    } else if (fit.status === "tight") {
      label += ` [⚠️ Tight: leaves ${fit.headroomGb.toFixed(1)}GB headroom]`;
    } else {
      label += ` [✅ Comfortable fit]`;
    }

    return {
      name: label,
      value: model.modelId,
      checked: current.includes(model.modelId),
      disabled,
    };
  });
}

function sttChoices(
  current: string[],
  vramGb: number,
): Array<{
  name: string;
  value: string;
  checked?: boolean;
  disabled?: string | boolean;
}> {
  return listModels("stt").map((model) => {
    const fit = evaluateModelFit(model, vramGb);
    let label = `${model.modelId} (${model.storageGb.toFixed(2)}GB, min VRAM ${model.minVramGb}GB)`;

    let disabled: string | boolean = false;
    if (fit.status === "insufficient") {
      label += ` [❌ Requires ${model.minVramGb}GB, you have ${vramGb}GB]`;
      disabled = `Requires ${model.minVramGb}GB VRAM`;
    } else if (fit.status === "tight") {
      label += ` [⚠️ Tight: leaves ${fit.headroomGb.toFixed(1)}GB headroom]`;
    } else {
      label += ` [✅ Comfortable fit]`;
    }

    return {
      name: label,
      value: model.modelId,
      checked: current.includes(model.modelId),
      disabled,
    };
  });
}

function imageChoices(
  current: string[],
  vramGb: number,
): Array<{
  name: string;
  value: string;
  checked?: boolean;
  disabled?: string | boolean;
}> {
  return listModels("image").map((model) => {
    const fit = evaluateModelFit(model, vramGb);
    let label = `${model.modelId} (${model.storageGb.toFixed(2)}GB, min VRAM ${model.minVramGb}GB)`;

    let disabled: string | boolean = false;
    if (fit.status === "insufficient") {
      label += ` [❌ Requires ${model.minVramGb}GB, you have ${vramGb}GB]`;
      disabled = `Requires ${model.minVramGb}GB VRAM`;
    } else if (fit.status === "tight") {
      label += ` [⚠️ Tight: leaves ${fit.headroomGb.toFixed(1)}GB headroom]`;
    } else {
      label += ` [✅ Comfortable fit]`;
    }

    return {
      name: label,
      value: model.modelId,
      checked: current.includes(model.modelId),
      disabled,
    };
  });
}

async function interactiveConfigureSelective(
  config: LocalBaseConfig,
  locked: Set<keyof LocalBaseConfig>,
  vramGb: number,
): Promise<LocalBaseConfig> {
  console.log("\nInteractive setup mode");

  const useAll =
    !locked.has("root") &&
    !locked.has("host") &&
    !locked.has("port") &&
    !locked.has("ctxSize") &&
    !locked.has("sttHost") &&
    !locked.has("sttPort") &&
    !locked.has("startupOnBoot") &&
    !locked.has("selectedLlmModels") &&
    !locked.has("selectedSttModels") &&
    !locked.has("activeLlmModel") &&
    !locked.has("activeSttModel");

  if (!locked.has("root"))
    config.root = await textPrompt("Root directory", config.root);
  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;

  if (!locked.has("host"))
    config.host = await textPrompt("LLM host", config.host);
  if (!locked.has("port"))
    config.port = await numberPrompt("LLM port", config.port);

  if (!locked.has("sttHost"))
    config.sttHost = await textPrompt("STT host", config.sttHost);
  if (!locked.has("sttPort"))
    config.sttPort = await numberPrompt("STT port", config.sttPort);

  if (!locked.has("startupOnBoot")) {
    config.startupOnBoot = await confirmPrompt(
      "Start services on boot",
      config.startupOnBoot,
    );
  }

  if (!locked.has("selectedLlmModels")) {
    config.selectedLlmModels =
      validateModelList(
        await multiSelectPrompt(
          "Select LLM models",
          llmChoices(config.selectedLlmModels, vramGb),
          true,
        ),
        "llm",
      ) ?? config.selectedLlmModels;
  }

  if (!locked.has("activeLlmModel")) {
    const options = config.selectedLlmModels.map((id) => ({
      name: id,
      value: id,
    }));
    const fallback = options[0]?.value ?? config.activeLlmModel;
    config.activeLlmModel = await singleSelectPrompt(
      "Active LLM model",
      options,
      fallback,
    );
  }

  if (!locked.has("ctxSize")) {
    const spec = byId(config.activeLlmModel);
    const recommendedCtx = spec
      ? calculateMaxSafeContextSize(spec, vramGb)
      : vramGb >= 32
        ? 32768
        : 8192;
    let suggestCtx = config.ctxSize;
    if (
      config.ctxSize <= 8192 ||
      config.ctxSize === 32768 ||
      recommendedCtx > config.ctxSize
    ) {
      suggestCtx = recommendedCtx;
    }
    config.ctxSize = await numberPrompt(
      `LLM maximum context limit (ceiling for dynamic sizing; recommended for ${config.activeLlmModel}: ${recommendedCtx})`,
      suggestCtx,
    );
  }

  if (!locked.has("parallel")) {
    while (true) {
      const value = await textPrompt(
        PARALLEL_SLOTS_PROMPT,
        String(config.parallel),
      );
      try {
        config.parallel = parseParallelSlots(value);
        break;
      } catch (error) {
        console.log((error as Error).message);
      }
    }
  }

  if (!locked.has("selectedSttModels")) {
    config.selectedSttModels =
      validateModelList(
        await multiSelectPrompt(
          "Select STT models (select none to disable)",
          sttChoices(config.selectedSttModels, vramGb),
          false,
        ),
        "stt",
      ) ?? config.selectedSttModels;
  }

  if (!locked.has("activeSttModel")) {
    if (config.selectedSttModels.length > 0) {
      const options = config.selectedSttModels.map((id) => ({
        name: id,
        value: id,
      }));
      const fallback = options
        .map((o) => o.value)
        .includes(config.activeSttModel)
        ? config.activeSttModel
        : options[0].value;
      config.activeSttModel = await singleSelectPrompt(
        "Active STT model",
        options,
        fallback,
      );
    } else {
      config.activeSttModel = "";
    }
  }

  if (!locked.has("selectedImageModels")) {
    config.selectedImageModels =
      validateModelList(
        await multiSelectPrompt(
          "Select Image models (select none to disable)",
          imageChoices(config.selectedImageModels, vramGb),
          false,
        ),
        "image",
      ) ?? config.selectedImageModels;
  }

  if (!locked.has("activeImageModel")) {
    if (config.selectedImageModels.length > 0) {
      const options = config.selectedImageModels.map((id) => ({
        name: id,
        value: id,
      }));
      const fallback = options.some(
        (option) => option.value === config.activeImageModel,
      )
        ? config.activeImageModel
        : options[0].value;
      config.activeImageModel = await singleSelectPrompt(
        "Active Image model",
        options,
        fallback,
      );
    } else {
      config.activeImageModel = "";
    }
  }

  if (!locked.has("hfToken")) {
    config.hfToken = await textPrompt(
      "Hugging Face access token (optional, for gated models like Gemma/Llama)",
      config.hfToken || process.env.HF_TOKEN || "",
    );
  }

  if (useAll)
    console.log(
      "\nTip: run `local-base catalog --kind <kind>` for full model details before final install.",
    );
  return config;
}

export async function syncContinueConfig(
  config: LocalBaseConfig,
  activeModelCtxSizeOverride?: number,
): Promise<void> {
  // Gateway tests must not mutate a contributor's Continue configuration.
  if (process.env.LOCALBASE_TEST_DISABLE_CONTINUE_SYNC === "1") return;

  const continueDir = join(homedir(), ".continue");
  const configPath = join(continueDir, "config.json");

  if (!(await Bun.file(configPath).exists())) {
    return;
  }

  try {
    const raw = Bun.file(configPath);
    const text = await raw.text();
    const cleaned = text.replace(
      /("([^"\\]|\\.)*")|(\/\/[^\n]*|\/\*[\s\S]*?\*\/)/g,
      (_match, g1) => {
        if (g1) return g1;
        return "";
      },
    );
    const data = continueConfigSchema.parse(JSON.parse(cleaned));
    const models = data.models ?? [];

    const host = config.host === "0.0.0.0" ? "localhost" : config.host;
    const wrapperPort = 2273;
    const apiKey = process.env.LOCALBASE_API_KEY || "";

    // Filter out existing LocalBase model entries to avoid duplicates
    data.models = models.filter((model) => {
      const title = continueField(model, "title").toLowerCase();
      const apiBase = continueField(model, "apiBase").toLowerCase();
      return (
        !title.includes("localbase") &&
        !apiBase.includes(":2273/v1") &&
        !apiBase.includes(":18787/v1") &&
        !apiBase.includes(":8787/v1") &&
        !apiBase.includes("local-base")
      );
    });

    const activeModel = config.activeLlmModel;
    const vramGb = (await detectSpecs()).gpuVramGb;

    // Ensure the active model is placed at the front of the list
    const sortedSelectedModels = [
      activeModel,
      ...config.selectedLlmModels.filter((m) => m !== activeModel),
    ];

    for (const modelId of sortedSelectedModels) {
      if (!config.selectedLlmModels.includes(modelId)) continue;
      const spec = byId(modelId);
      const displayName = spec
        ? `LocalBase (${spec.family} ${spec.version})`
        : `LocalBase (${modelId})`;
      const recommendedCtx = spec
        ? calculateMaxSafeContextSize(spec, vramGb)
        : config.ctxSize;
      const actualCtx =
        modelId === activeModel
          ? (activeModelCtxSizeOverride ?? config.ctxSize)
          : Math.min(recommendedCtx, config.ctxSize);

      data.models.unshift({
        title: displayName,
        provider: "openai",
        model: modelId,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined,
        completionOptions: {
          contextLength: actualCtx,
        },
      });
    }

    // Configure tab autocomplete if not set or if pointing to LocalBase
    const currentTabTitle = continueField(
      data.tabAutocompleteModel,
      "title",
    ).toLowerCase();
    const currentTabBase = continueField(
      data.tabAutocompleteModel,
      "apiBase",
    ).toLowerCase();
    if (
      !data.tabAutocompleteModel ||
      currentTabTitle.includes("localbase") ||
      currentTabBase.includes(":2273/v1") ||
      currentTabBase.includes(":18787/v1") ||
      currentTabBase.includes(":8787/v1")
    ) {
      data.tabAutocompleteModel = {
        title: `LocalBase Autocomplete (${activeModel})`,
        provider: "openai",
        model: activeModel,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined,
      };
    }

    // Configure embeddings provider if not set or if pointing to LocalBase
    const currentEmbeddingsProvider = continueField(
      data.embeddingsProvider,
      "provider",
    ).toLowerCase();
    const currentEmbeddingsBase = continueField(
      data.embeddingsProvider,
      "apiBase",
    ).toLowerCase();
    if (
      !data.embeddingsProvider ||
      currentEmbeddingsProvider === "openai" ||
      currentEmbeddingsBase.includes(":2273/v1") ||
      currentEmbeddingsBase.includes(":18787/v1") ||
      currentEmbeddingsBase.includes(":8787/v1")
    ) {
      data.embeddingsProvider = {
        provider: "openai",
        model: activeModel,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined,
      };
    }

    await Bun.write(configPath, JSON.stringify(data, null, 2));
    console.log(
      `\n🔄 Automatically synchronized model configs and autocomplete/embeddings to ${configPath}`,
    );
  } catch (err) {
    console.warn(
      "\n⚠️  Could not automatically synchronize config with Continue:",
      (err as Error).message,
    );
  }
}

export async function runConfigure(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const specs = ctx.specs;
  const flags = parseConfigureFlags(args);
  const rawToml = flags.configPath
    ? await loadTomlOverrides(flags.configPath)
    : {};
  const root = flags.root ?? rawToml.root;
  const hasConfig = await Bun.file(
    root ? `${root}/local-base.db` : `${defaultRoot()}/local-base.db`,
  ).exists();

  let config = root
    ? loadConfig(ctx.database, root, specs.gpuVramGb)
    : ctx.config;
  const llmFromFlags = validateModelList(parseList(flags.llmModels), "llm");
  const sttFromFlags = validateModelList(parseList(flags.sttModels), "stt");
  const imageFromFlags = validateModelList(
    parseList(flags.imageModels),
    "image",
  );
  const llmFromToml = validateModelList(rawToml.selectedLlmModels, "llm");
  const sttFromToml = validateModelList(rawToml.selectedSttModels, "stt");
  const imageFromToml = validateModelList(rawToml.selectedImageModels, "image");
  const parallelFromFlag = flags.parallel;
  const parallelInput = parallelFromFlag ?? rawToml.parallel;
  const parallel =
    parallelInput === undefined
      ? config.parallel
      : parseParallelSlots(parallelInput);

  const locked = new Set<keyof LocalBaseConfig>();
  const maybeLock = (key: keyof LocalBaseConfig, value: unknown): void => {
    if (value !== undefined) locked.add(key);
  };

  maybeLock("root", flags.root ?? rawToml.root);
  maybeLock("host", flags.host ?? rawToml.host);
  maybeLock("port", flags.port ?? rawToml.port);
  maybeLock("ctxSize", flags.ctxSize ?? rawToml.ctxSize);
  maybeLock("parallel", parallelInput);
  maybeLock("sttHost", flags.sttHost ?? rawToml.sttHost);
  maybeLock("sttPort", flags.sttPort ?? rawToml.sttPort);
  maybeLock("startupOnBoot", flags.startupOnBoot ?? rawToml.startupOnBoot);
  maybeLock("selectedLlmModels", llmFromFlags ?? llmFromToml);
  maybeLock("selectedSttModels", sttFromFlags ?? sttFromToml);
  maybeLock("selectedImageModels", imageFromFlags ?? imageFromToml);
  maybeLock("activeLlmModel", flags.activeLlm ?? rawToml.activeLlmModel);
  maybeLock("activeSttModel", flags.activeStt ?? rawToml.activeSttModel);
  maybeLock("activeImageModel", flags.activeImage ?? rawToml.activeImageModel);
  maybeLock("hfToken", flags.hfToken ?? rawToml.hfToken);

  config = {
    ...config,
    root: flags.root ?? rawToml.root ?? config.root,
    host: flags.host ?? rawToml.host ?? config.host,
    port: toInt(flags.port, rawToml.port ?? config.port),
    ctxSize: toInt(flags.ctxSize, rawToml.ctxSize ?? config.ctxSize),
    parallel,
    sttHost: flags.sttHost ?? rawToml.sttHost ?? config.sttHost,
    sttPort: toInt(flags.sttPort, rawToml.sttPort ?? config.sttPort),
    startupOnBoot: parseBool(
      flags.startupOnBoot,
      rawToml.startupOnBoot ?? config.startupOnBoot,
    ),
    selectedLlmModels: llmFromFlags ?? llmFromToml ?? config.selectedLlmModels,
    selectedSttModels: sttFromFlags ?? sttFromToml ?? config.selectedSttModels,
    selectedImageModels:
      imageFromFlags ?? imageFromToml ?? config.selectedImageModels,
    activeLlmModel:
      flags.activeLlm ?? rawToml.activeLlmModel ?? config.activeLlmModel,
    activeSttModel:
      flags.activeStt ?? rawToml.activeSttModel ?? config.activeSttModel,
    activeImageModel:
      flags.activeImage ?? rawToml.activeImageModel ?? config.activeImageModel,
    hfToken:
      flags.hfToken ??
      rawToml.hfToken ??
      config.hfToken ??
      process.env.HF_TOKEN ??
      "",
  };

  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;
  config.imageModelsDir = `${config.root}/models/image`;

  const explicitMode =
    flags.all ||
    flags.defaults ||
    flags.configPath !== undefined ||
    locked.size > 0;
  const shouldAsk =
    flags.all || (!flags.defaults && (!hasConfig || !explicitMode));
  if (shouldAsk)
    config = await interactiveConfigureSelective(
      config,
      locked,
      specs.gpuVramGb,
    );

  if (byId(config.activeLlmModel)?.kind !== "llm")
    throw new Error(`Active LLM model is invalid: ${config.activeLlmModel}`);
  if (config.activeSttModel && byId(config.activeSttModel)?.kind !== "stt")
    throw new Error(`Active STT model is invalid: ${config.activeSttModel}`);
  if (
    config.activeImageModel &&
    byId(config.activeImageModel)?.kind !== "image"
  )
    throw new Error(
      `Active Image model is invalid: ${config.activeImageModel}`,
    );

  warnAboutParallelOomRisk(config.parallel, specs.gpuVramGb);

  saveConfig(ctx.database, config);
  await syncContinueConfig(config);
  console.log(`Saved configuration to ${config.root}/local-base.db`);
  console.log(`Selected LLM models: ${config.selectedLlmModels.join(", ")}`);
  console.log(`Selected STT models: ${config.selectedSttModels.join(", ")}`);
  console.log(
    `Selected Image models: ${config.selectedImageModels.join(", ")}`,
  );

  const hasAnyKeys = loadApiKeys(ctx.database, config).some(
    (k) => !k.revokedAt,
  );
  const createKeyFlag = flags.createKey;
  let createFirstKey = parseBool(createKeyFlag, true);
  if (createKeyFlag === undefined && shouldAsk && !hasAnyKeys) {
    createFirstKey = await confirmPrompt(
      "No API keys found. Create one now",
      true,
    );
  }

  if (!hasAnyKeys && createFirstKey) {
    const { record, rawKey } = createApiKey(ctx.database, config, "default");
    console.log("\nCreated initial API key:");
    console.log(`id=${record.id} name=${record.name} prefix=${record.prefix}`);
    console.log(`secret=${rawKey}`);
    console.log("Store this secret now. It is not shown again.");
  }

  return 0;
}
