import { existsSync } from "node:fs";
import { byId, recommendedForVram, recommendedSttForVram } from "../../../catalog";
import { createApiKey, defaultRoot, loadApiKeys, loadConfig, saveConfig, type LocalBaseConfig } from "../../../manager";
import { detectSpecs } from "../../../system";
import { validateModelList } from "../../models/model-selection";
import { parseBool, parseFlag, parseList, toInt } from "../../../utils/args";
import { confirmPrompt, multiSelectPrompt, numberPrompt, singleSelectPrompt, textPrompt } from "../../../utils/prompt";
import { loadTomlOverrides } from "../../../utils/toml";

function llmChoices(current: string[]): Array<{ name: string; value: string; checked?: boolean }> {
  return recommendedForVram(detectSpecs().gpuVramGb).slice(0, 12).map((model) => ({
    name: `${model.modelId} (${model.storageGb.toFixed(2)}GB, min VRAM ${model.minVramGb}GB, coding ${model.codingScore}/10)`,
    value: model.modelId,
    checked: current.includes(model.modelId)
  }));
}

function sttChoices(current: string[]): Array<{ name: string; value: string; checked?: boolean }> {
  return recommendedSttForVram(detectSpecs().gpuVramGb).slice(0, 12).map((model) => ({
    name: `${model.modelId} (${model.storageGb.toFixed(2)}GB, min VRAM ${model.minVramGb}GB)`,
    value: model.modelId,
    checked: current.includes(model.modelId)
  }));
}

async function interactiveConfigureSelective(config: LocalBaseConfig, locked: Set<keyof LocalBaseConfig>): Promise<LocalBaseConfig> {
  console.log("\nInteractive setup mode");

  const useAll = !locked.has("root") && !locked.has("host") && !locked.has("port") && !locked.has("ctxSize") && !locked.has("sttHost") && !locked.has("sttPort") && !locked.has("startupOnBoot") && !locked.has("selectedLlmModels") && !locked.has("selectedSttModels") && !locked.has("activeLlmModel") && !locked.has("activeSttModel");

  if (!locked.has("root")) config.root = await textPrompt("Root directory", config.root);
  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;

  if (!locked.has("host")) config.host = await textPrompt("LLM host", config.host);
  if (!locked.has("port")) config.port = await numberPrompt("LLM port", config.port);
  if (!locked.has("ctxSize")) config.ctxSize = await numberPrompt("LLM context size", config.ctxSize);

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

  if (!locked.has("selectedSttModels")) {
    config.selectedSttModels = validateModelList(await multiSelectPrompt("Select STT models", sttChoices(config.selectedSttModels)), "stt") ?? config.selectedSttModels;
  }

  if (!locked.has("activeSttModel")) {
    const options = config.selectedSttModels.map((id) => ({ name: id, value: id }));
    const fallback = options[0]?.value ?? config.activeSttModel;
    config.activeSttModel = await singleSelectPrompt("Active STT model", options, fallback);
  }

  if (useAll) console.log("\nTip: run `local-base catalog --kind <kind>` for full model details before final install.");
  return config;
}

export async function runConfigure(args: string[]): Promise<number> {
  const specs = detectSpecs();
  const configPath = parseFlag(args, "--config");
  const rawToml = configPath ? loadTomlOverrides(configPath) : {};
  const root = parseFlag(args, "--root") ?? rawToml.root;
  const hasConfig = root ? existsSync(`${root}/local-base.db`) : existsSync(`${defaultRoot()}/local-base.db`);

  let config = loadConfig(root, specs.gpuVramGb);
  const llmFromFlags = validateModelList(parseList(parseFlag(args, "--llm-models")), "llm");
  const sttFromFlags = validateModelList(parseList(parseFlag(args, "--stt-models")), "stt");
  const llmFromToml = validateModelList(rawToml.selectedLlmModels, "llm");
  const sttFromToml = validateModelList(rawToml.selectedSttModels, "stt");

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
  maybeLock("activeLlmModel", parseFlag(args, "--active-llm") ?? rawToml.activeLlmModel);
  maybeLock("activeSttModel", parseFlag(args, "--active-stt") ?? rawToml.activeSttModel);

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
    activeLlmModel: parseFlag(args, "--active-llm") ?? rawToml.activeLlmModel ?? config.activeLlmModel,
    activeSttModel: parseFlag(args, "--active-stt") ?? rawToml.activeSttModel ?? config.activeSttModel
  };

  config.llmModelsDir = `${config.root}/models/llm`;
  config.sttModelsDir = `${config.root}/models/stt`;

  const explicitMode = args.includes("--all") || args.includes("--defaults") || args.includes("--config") || locked.size > 0;
  const shouldAsk = args.includes("--all") || (!args.includes("--defaults") && (!hasConfig || !explicitMode));
  if (shouldAsk) config = await interactiveConfigureSelective(config, locked);

  if (byId(config.activeLlmModel)?.kind !== "llm") throw new Error(`Active LLM model is invalid: ${config.activeLlmModel}`);
  if (byId(config.activeSttModel)?.kind !== "stt") throw new Error(`Active STT model is invalid: ${config.activeSttModel}`);

  saveConfig(config);
  console.log(`Saved configuration to ${config.root}/local-base.db`);
  console.log(`Selected LLM models: ${config.selectedLlmModels.join(", ")}`);
  console.log(`Selected STT models: ${config.selectedSttModels.join(", ")}`);

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
