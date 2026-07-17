import { existsSync } from "node:fs";
import { join } from "node:path";
import { homedir } from "node:os";
import { byId, listModels, evaluateModelFit, calculateMaxSafeContextSize } from "../../../catalog";
import { createApiKey, defaultRoot, loadApiKeys, loadConfig, saveConfig, type LocalBaseConfig } from "../../../manager";
import type { AppContext } from "../../../context";
import { detectSpecs } from "../../../system";
import { validateModelList } from "../../models/model-selection";
import { parseBool, parseFlag, parseList, toInt } from "../../../utils/args";
import { confirmPrompt, multiSelectPrompt, numberPrompt, singleSelectPrompt, textPrompt } from "../../../utils/prompt";
import { loadTomlOverrides } from "../../../utils/toml";

function llmChoices(current: string[]): Array<{ name: string; value: string; checked?: boolean; disabled?: string | boolean }> {
  const vramGb = detectSpecs().gpuVramGb;
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
      disabled
    };
  });
}

function sttChoices(current: string[]): Array<{ name: string; value: string; checked?: boolean; disabled?: string | boolean }> {
  const vramGb = detectSpecs().gpuVramGb;
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
      disabled
    };
  });
}

function imageChoices(current: string[]): Array<{ name: string; value: string; checked?: boolean; disabled?: string | boolean }> {
  const vramGb = detectSpecs().gpuVramGb;
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
      disabled
    };
  });
}


async function interactiveConfigureSelective(config: LocalBaseConfig, locked: Set<keyof LocalBaseConfig>, vramGb: number): Promise<LocalBaseConfig> {
  console.log("\nInteractive setup mode");

  const useAll = !locked.has("root") && !locked.has("host") && !locked.has("port") && !locked.has("ctxSize") && !locked.has("sttHost") && !locked.has("sttPort") && !locked.has("startupOnBoot") && !locked.has("selectedLlmModels") && !locked.has("selectedSttModels") && !locked.has("activeLlmModel") && !locked.has("activeSttModel");

  if (!locked.has("root")) config.root = await textPrompt("Root directory", config.root);
  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;

  if (!locked.has("host")) config.host = await textPrompt("LLM host", config.host);
  if (!locked.has("port")) config.port = await numberPrompt("LLM port", config.port);

  if (!locked.has("sttHost")) config.sttHost = await textPrompt("STT host", config.sttHost);
  if (!locked.has("sttPort")) config.sttPort = await numberPrompt("STT port", config.sttPort);

  if (!locked.has("startupOnBoot")) {
    config.startupOnBoot = await confirmPrompt("Start services on boot", config.startupOnBoot);
  }

  if (!locked.has("selectedLlmModels")) {
    config.selectedLlmModels = validateModelList(await multiSelectPrompt("Select LLM models", llmChoices(config.selectedLlmModels)), "llm") ?? config.selectedLlmModels;
  }

  if (!locked.has("activeLlmModel")) {
    const options = config.selectedLlmModels.map((id) => ({ name: id, value: id }));
    const fallback = options[0]?.value ?? config.activeLlmModel;
    config.activeLlmModel = await singleSelectPrompt("Active LLM model", options, fallback);
  }

  if (!locked.has("ctxSize")) {
    const spec = byId(config.activeLlmModel);
    const recommendedCtx = spec ? calculateMaxSafeContextSize(spec, vramGb) : (vramGb >= 32 ? 32768 : 8192);
    let suggestCtx = config.ctxSize;
    if (config.ctxSize <= 8192 || config.ctxSize === 32768 || recommendedCtx > config.ctxSize) {
      suggestCtx = recommendedCtx;
    }
    config.ctxSize = await numberPrompt(`LLM maximum context limit (ceiling for dynamic sizing; recommended for ${config.activeLlmModel}: ${recommendedCtx})`, suggestCtx);
  }

  if (!locked.has("selectedSttModels")) {
    config.selectedSttModels = validateModelList(await multiSelectPrompt("Select STT models (select none to disable)", sttChoices(config.selectedSttModels)), "stt") ?? config.selectedSttModels;
  }

  if (!locked.has("activeSttModel")) {
    if (config.selectedSttModels.length > 0) {
      const options = config.selectedSttModels.map((id) => ({ name: id, value: id }));
      const fallback = options.map(o => o.value).includes(config.activeSttModel) ? config.activeSttModel : options[0].value;
      config.activeSttModel = await singleSelectPrompt("Active STT model", options, fallback);
    } else {
      config.activeSttModel = "";
    }
  }

  if (!locked.has("selectedImageModels")) {
    config.selectedImageModels = validateModelList(await multiSelectPrompt("Select Image models (select none to disable)", imageChoices(config.selectedImageModels)), "image") ?? config.selectedImageModels;
  }

  if (!locked.has("activeImageModel")) {
    if (config.selectedImageModels.length > 0) {
      const options = config.selectedImageModels.map((id) => ({ name: id, value: id }));
      const fallback = options.includes(config.activeImageModel as any) ? config.activeImageModel : options[0].value;
      config.activeImageModel = await singleSelectPrompt("Active Image model", options, fallback);
    } else {
      config.activeImageModel = "";
    }
  }

  if (useAll) console.log("\nTip: run `local-base catalog --kind <kind>` for full model details before final install.");
  return config;
}

export async function syncOpenCodeConfig(config: LocalBaseConfig, activeModelCtxSizeOverride?: number): Promise<void> {
  const opencodeDir = join(homedir(), ".config", "opencode");
  const configPath = join(opencodeDir, "opencode.jsonc");
  const jsonPath = join(opencodeDir, "opencode.json");

  let path = "";
  if (existsSync(configPath)) {
    path = configPath;
  } else if (existsSync(jsonPath)) {
    path = jsonPath;
  } else {
    return;
  }

  try {
    const raw = Bun.file(path);
    const text = await raw.text();
    const cleaned = text.replace(/("([^"\\]|\\.)*")|(\/\/[^\n]*|\/\*[\s\S]*?\*\/)/g, (m, g1) => {
      if (g1) return g1;
      return "";
    });
    const data = JSON.parse(cleaned);

    if (!data.provider) data.provider = {};

    const host = config.host === "0.0.0.0" ? "localhost" : config.host;
    const wrapperPort = 2273;

    data.provider.localbase = {
      npm: "@ai-sdk/openai-compatible",
      name: "LocalBase",
      options: {
        baseURL: `http://${host}:${wrapperPort}/v1`
      },
      models: {}
    };

    const activeModel = config.activeLlmModel;
    const vramGb = detectSpecs().gpuVramGb;

    for (const modelId of config.selectedLlmModels) {
      const spec = byId(modelId);
      const displayName = spec ? `${spec.family} ${spec.version}` : modelId;
      const recommendedCtx = spec ? calculateMaxSafeContextSize(spec, vramGb) : config.ctxSize;

      const actualCtx = modelId === activeModel
        ? (activeModelCtxSizeOverride ?? config.ctxSize)
        : Math.min(recommendedCtx, config.ctxSize);

      data.provider.localbase.models[modelId] = {
        name: displayName,
        tool_call: true,
        limit: {
          context: actualCtx,
          output: 4096
        }
      };
    }

    data.model = `localbase/${activeModel}`;

    await Bun.write(path, JSON.stringify(data, null, 2));
    console.log(`\n🔄 Automatically synchronized model configs and context limits to ${path}`);
  } catch (err) {
    console.warn("\n⚠️  Could not automatically synchronize config with OpenCode:", (err as Error).message);
  }
}

export async function syncContinueConfig(config: LocalBaseConfig, activeModelCtxSizeOverride?: number): Promise<void> {
  const continueDir = join(homedir(), ".continue");
  const configPath = join(continueDir, "config.json");

  if (!existsSync(configPath)) {
    return;
  }

  try {
    const raw = Bun.file(configPath);
    const text = await raw.text();
    const cleaned = text.replace(/("([^"\\]|\\.)*")|(\/\/[^\n]*|\/\*[\s\S]*?\*\/)/g, (m, g1) => {
      if (g1) return g1;
      return "";
    });
    const data = JSON.parse(cleaned);

    if (!Array.isArray(data.models)) {
      data.models = [];
    }

    const host = config.host === "0.0.0.0" ? "localhost" : config.host;
    const wrapperPort = 2273;
    const apiKey = process.env.LOCALBASE_API_KEY || "";

    // Filter out existing LocalBase model entries to avoid duplicates
    data.models = data.models.filter((m: any) => {
      if (!m || typeof m !== "object") return true;
      const title = String(m.title || "").toLowerCase();
      const apiBase = String(m.apiBase || "").toLowerCase();
      return !title.includes("localbase") && !apiBase.includes(":2273/v1") && !apiBase.includes(":18787/v1") && !apiBase.includes(":8787/v1") && !apiBase.includes("local-base");
    });

    const activeModel = config.activeLlmModel;
    const vramGb = detectSpecs().gpuVramGb;

    // Ensure the active model is placed at the front of the list
    const sortedSelectedModels = [
      activeModel,
      ...config.selectedLlmModels.filter(m => m !== activeModel)
    ];

    for (const modelId of sortedSelectedModels) {
      if (!config.selectedLlmModels.includes(modelId)) continue;
      const spec = byId(modelId);
      const displayName = spec ? `LocalBase (${spec.family} ${spec.version})` : `LocalBase (${modelId})`;
      const recommendedCtx = spec ? calculateMaxSafeContextSize(spec, vramGb) : config.ctxSize;
      const actualCtx = modelId === activeModel
        ? (activeModelCtxSizeOverride ?? config.ctxSize)
        : Math.min(recommendedCtx, config.ctxSize);

      data.models.unshift({
        title: displayName,
        provider: "openai",
        model: modelId,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined,
        completionOptions: {
          contextLength: actualCtx
        }
      });
    }

    // Configure tab autocomplete if not set or if pointing to LocalBase
    const currentTabTitle = String(data.tabAutocompleteModel?.title || "").toLowerCase();
    const currentTabBase = String(data.tabAutocompleteModel?.apiBase || "").toLowerCase();
    if (!data.tabAutocompleteModel || currentTabTitle.includes("localbase") || currentTabBase.includes(":2273/v1") || currentTabBase.includes(":18787/v1") || currentTabBase.includes(":8787/v1")) {
      data.tabAutocompleteModel = {
        title: `LocalBase Autocomplete (${activeModel})`,
        provider: "openai",
        model: activeModel,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined
      };
    }

    // Configure embeddings provider if not set or if pointing to LocalBase
    const currentEmbeddingsProvider = String(data.embeddingsProvider?.provider || "").toLowerCase();
    const currentEmbeddingsBase = String(data.embeddingsProvider?.apiBase || "").toLowerCase();
    if (!data.embeddingsProvider || currentEmbeddingsProvider === "openai" || currentEmbeddingsBase.includes(":2273/v1") || currentEmbeddingsBase.includes(":18787/v1") || currentEmbeddingsBase.includes(":8787/v1")) {
      data.embeddingsProvider = {
        provider: "openai",
        model: activeModel,
        apiBase: `http://${host}:${wrapperPort}/v1`,
        apiKey: apiKey || undefined
      };
    }

    await Bun.write(configPath, JSON.stringify(data, null, 2));
    console.log(`\n🔄 Automatically synchronized model configs and autocomplete/embeddings to ${configPath}`);
  } catch (err) {
    console.warn("\n⚠️  Could not automatically synchronize config with Continue:", (err as Error).message);
  }
}

export async function runConfigure(args: string[], ctx: AppContext): Promise<number> {
  const specs = ctx.specs;
  const configPath = parseFlag(args, "--config");
  const rawToml = configPath ? loadTomlOverrides(configPath) : {};
  const root = parseFlag(args, "--root") ?? rawToml.root;
  const hasConfig = root ? existsSync(`${root}/local-base.db`) : existsSync(`${defaultRoot()}/local-base.db`);

  let config = root ? loadConfig(root, specs.gpuVramGb) : ctx.config;
  const llmFromFlags = validateModelList(parseList(parseFlag(args, "--llm-models")), "llm");
  const sttFromFlags = validateModelList(parseList(parseFlag(args, "--stt-models")), "stt");
  const imageFromFlags = validateModelList(parseList(parseFlag(args, "--image-models")), "image");
  const llmFromToml = validateModelList(rawToml.selectedLlmModels, "llm");
  const sttFromToml = validateModelList(rawToml.selectedSttModels, "stt");
  const imageFromToml = validateModelList(rawToml.selectedImageModels, "image");

  const locked = new Set<keyof LocalBaseConfig>();
  const maybeLock = (key: keyof LocalBaseConfig, value: unknown): void => {
    if (value !== undefined) locked.add(key);
  };

  maybeLock("root", parseFlag(args, "--root") ?? rawToml.root);
  maybeLock("host", parseFlag(args, "--host") ?? rawToml.host);
  maybeLock("port", parseFlag(args, "--port") ?? rawToml.port);
  maybeLock("ctxSize", parseFlag(args, "--ctx-size") ?? rawToml.ctxSize);
  maybeLock("sttHost", parseFlag(args, "--stt-host") ?? rawToml.sttHost);
  maybeLock("sttPort", parseFlag(args, "--stt-port") ?? rawToml.sttPort);
  maybeLock("startupOnBoot", parseFlag(args, "--startup-on-boot") ?? rawToml.startupOnBoot);
  maybeLock("selectedLlmModels", llmFromFlags ?? llmFromToml);
  maybeLock("selectedSttModels", sttFromFlags ?? sttFromToml);
  maybeLock("selectedImageModels", imageFromFlags ?? imageFromToml);
  maybeLock("activeLlmModel", parseFlag(args, "--active-llm") ?? rawToml.activeLlmModel);
  maybeLock("activeSttModel", parseFlag(args, "--active-stt") ?? rawToml.activeSttModel);
  maybeLock("activeImageModel", parseFlag(args, "--active-image") ?? rawToml.activeImageModel);

  config = {
    ...config,
    root: parseFlag(args, "--root") ?? rawToml.root ?? config.root,
    host: parseFlag(args, "--host") ?? rawToml.host ?? config.host,
    port: toInt(parseFlag(args, "--port"), rawToml.port ?? config.port),
    ctxSize: toInt(parseFlag(args, "--ctx-size"), rawToml.ctxSize ?? config.ctxSize),
    sttHost: parseFlag(args, "--stt-host") ?? rawToml.sttHost ?? config.sttHost,
    sttPort: toInt(parseFlag(args, "--stt-port"), rawToml.sttPort ?? config.sttPort),
    startupOnBoot: parseBool(parseFlag(args, "--startup-on-boot"), rawToml.startupOnBoot ?? config.startupOnBoot),
    selectedLlmModels: llmFromFlags ?? llmFromToml ?? config.selectedLlmModels,
    selectedSttModels: sttFromFlags ?? sttFromToml ?? config.selectedSttModels,
    selectedImageModels: imageFromFlags ?? imageFromToml ?? config.selectedImageModels,
    activeLlmModel: parseFlag(args, "--active-llm") ?? rawToml.activeLlmModel ?? config.activeLlmModel,
    activeSttModel: parseFlag(args, "--active-stt") ?? rawToml.activeSttModel ?? config.activeSttModel,
    activeImageModel: parseFlag(args, "--active-image") ?? rawToml.activeImageModel ?? config.activeImageModel
  };

  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;
  config.imageModelsDir = `${config.root}/models/image`;

  const explicitMode = args.includes("--all") || args.includes("--defaults") || args.includes("--config") || locked.size > 0;
  const shouldAsk = args.includes("--all") || (!args.includes("--defaults") && (!hasConfig || !explicitMode));
  if (shouldAsk) config = await interactiveConfigureSelective(config, locked, specs.gpuVramGb);

  if (byId(config.activeLlmModel)?.kind !== "llm") throw new Error(`Active LLM model is invalid: ${config.activeLlmModel}`);
  if (config.activeSttModel && byId(config.activeSttModel)?.kind !== "stt") throw new Error(`Active STT model is invalid: ${config.activeSttModel}`);
  if (config.activeImageModel && byId(config.activeImageModel)?.kind !== "image") throw new Error(`Active Image model is invalid: ${config.activeImageModel}`);

  saveConfig(config);
  await syncOpenCodeConfig(config);
  await syncContinueConfig(config);
  console.log(`Saved configuration to ${config.root}/local-base.db`);
  console.log(`Selected LLM models: ${config.selectedLlmModels.join(", ")}`);
  console.log(`Selected STT models: ${config.selectedSttModels.join(", ")}`);
  console.log(`Selected Image models: ${config.selectedImageModels.join(", ")}`);

  const hasAnyKeys = loadApiKeys(config).some((k) => !k.revokedAt);
  const createKeyFlag = parseFlag(args, "--create-key");
  let createFirstKey = parseBool(createKeyFlag, true);
  if (createKeyFlag === undefined && shouldAsk && !hasAnyKeys) {
    createFirstKey = await confirmPrompt("No API keys found. Create one now", true);
  }

  if (!hasAnyKeys && createFirstKey) {
    const { record, rawKey } = createApiKey(config, "default");
    console.log("\nCreated initial API key:");
    console.log(`id=${record.id} name=${record.name} prefix=${record.prefix}`);
    console.log(`secret=${rawKey}`);
    console.log("Store this secret now. It is not shown again.");
  }

  return 0;
}
