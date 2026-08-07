import {
  byId,
  listModels,
  evaluateModelFit,
  calculateMaxSafeContextSize,
} from "../../../catalog";
import {
  createApiKey,
  loadApiKeys,
  loadConfig,
  modelDirectories,
  saveConfig,
  type LocalBaseConfig,
} from "../../../manager";
import {
  parseEnvironmentOverrides,
  resolveEffectiveRoot,
  type AppContext,
} from "../../../context";
import {
  modelConfigurationSchema,
  validateModelList,
} from "../../models/model-selection";
import {
  confirmPrompt,
  multiSelectPrompt,
  numberPrompt,
  singleSelectPrompt,
  textPrompt,
} from "../../../utils/prompt";
import { loadTomlOverrides } from "../../../utils/toml";
import { parseParallelSlots } from "../parallel";
import { CliInputError, formatZodError } from "../../app/commands/errors";
import type { CommandExecution } from "../../app/commands/framework";
import type { ConfigureInput } from "../../app/commands/inputs";
import { publicApiKey, publicConfiguration } from "../../app/commands/results";
import { resolveOtelConfiguration } from "../../observability/otel";
import { memorySafetyConfigSchema } from "../../runtime/memory-safety";

export const PARALLEL_SLOTS_PROMPT =
  "Parallel request slots count (type 'auto' for dynamic auto-allocation, or an integer like 1, 2, 4)";

function validateExternalModelList(
  modelIds: string[] | undefined,
  kind: "llm" | "stt" | "image",
): string[] | undefined {
  try {
    return validateModelList(modelIds, kind);
  } catch (error) {
    throw new CliInputError(
      error instanceof Error ? error.message : String(error),
    );
  }
}

function validateComposedModelConfiguration(config: LocalBaseConfig): void {
  const result = modelConfigurationSchema.safeParse({
    selectedLlmModels: config.selectedLlmModels,
    selectedSttModels: config.selectedSttModels,
    selectedImageModels: config.selectedImageModels,
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel,
    activeImageModel: config.activeImageModel,
  });
  if (!result.success) throw new CliInputError(formatZodError(result.error));
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
    !locked.has("selectedLlmModels") &&
    !locked.has("selectedSttModels") &&
    !locked.has("activeLlmModel") &&
    !locked.has("activeSttModel");

  if (!locked.has("root"))
    config.root = await textPrompt("Root directory", config.root);

  if (!locked.has("host"))
    config.host = await textPrompt("LLM host", config.host);
  if (!locked.has("port"))
    config.port = await numberPrompt("LLM port", config.port);

  if (!locked.has("sttHost"))
    config.sttHost = await textPrompt("STT host", config.sttHost);
  if (!locked.has("sttPort"))
    config.sttPort = await numberPrompt("STT port", config.sttPort);

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
  if (!locked.has("otelEndpoint")) {
    config.otelEndpoint = await textPrompt(
      "OTLP/HTTP endpoint (empty disables export)",
      config.otelEndpoint,
    );
  }
  if (config.otelEndpoint && !locked.has("otelHeaders")) {
    config.otelHeaders = await textPrompt(
      "OTLP headers (key=value,...; optional)",
      config.otelHeaders,
    );
  }
  if (config.otelEndpoint && !locked.has("otelSampleRatio")) {
    config.otelSampleRatio = await numberPrompt(
      "Root trace sample percentage (0-100)",
      config.otelSampleRatio,
    );
  }

  if (useAll)
    console.log(
      "\nTip: run `local-base models catalog --kind <kind>` for full model details before final install.",
    );
  return config;
}

export async function runConfigure(
  flags: ConfigureInput,
  ctx: AppContext,
  execution: CommandExecution,
): Promise<{
  data: {
    configuration: ReturnType<typeof publicConfiguration>;
    createdKey?: ReturnType<typeof publicApiKey> & { secret: string };
  };
}> {
  const specs = ctx.specs;
  const rawToml = flags.configPath
    ? await loadTomlOverrides(flags.configPath)
    : {};
  const environment = parseEnvironmentOverrides(process.env);
  const configuredRoot = rawToml.root ?? ctx.config.root;
  const root = resolveEffectiveRoot(
    execution.global.root,
    environment.root,
    configuredRoot,
  );
  const hasConfig = await Bun.file(`${root}/local-base.db`).exists();

  let config = loadConfig(ctx.database, root, specs.gpuVramGb);
  const llmFromFlags = validateExternalModelList(flags.llmModels, "llm");
  const sttFromFlags = validateExternalModelList(flags.sttModels, "stt");
  const imageFromFlags = validateExternalModelList(flags.imageModels, "image");
  const llmFromToml = rawToml.selectedLlmModels;
  const sttFromToml = rawToml.selectedSttModels;
  const imageFromToml = rawToml.selectedImageModels;
  const parallelFromFlag = flags.parallel;
  const parallelInput = parallelFromFlag ?? rawToml.parallel;
  const parallel =
    parallelInput === undefined
      ? config.parallel
      : parseParallelSlots(parallelInput);
  const selectedLlmModels =
    llmFromFlags ?? llmFromToml ?? config.selectedLlmModels;
  const selectedSttModels =
    sttFromFlags ?? sttFromToml ?? config.selectedSttModels;
  const selectedImageModels =
    imageFromFlags ?? imageFromToml ?? config.selectedImageModels;
  const activeLlmModel =
    flags.activeLlm ??
    rawToml.activeLlmModel ??
    (selectedLlmModels.includes(config.activeLlmModel)
      ? config.activeLlmModel
      : selectedLlmModels[0]);
  const activeSttModel =
    flags.activeStt ??
    rawToml.activeSttModel ??
    (selectedSttModels.includes(config.activeSttModel)
      ? config.activeSttModel
      : (selectedSttModels[0] ?? ""));
  const activeImageModel =
    flags.activeImage ??
    rawToml.activeImageModel ??
    (selectedImageModels.includes(config.activeImageModel)
      ? config.activeImageModel
      : (selectedImageModels[0] ?? ""));

  const locked = new Set<keyof LocalBaseConfig>();
  const maybeLock = (key: keyof LocalBaseConfig, value: unknown): void => {
    if (value !== undefined) locked.add(key);
  };

  maybeLock("root", execution.global.root ?? environment.root ?? rawToml.root);
  maybeLock("host", flags.host ?? rawToml.host);
  maybeLock("port", flags.port ?? rawToml.port);
  maybeLock("ctxSize", flags.ctxSize ?? rawToml.ctxSize);
  maybeLock("parallel", parallelInput);
  maybeLock("sttHost", flags.sttHost ?? rawToml.sttHost);
  maybeLock("sttPort", flags.sttPort ?? rawToml.sttPort);
  maybeLock("selectedLlmModels", llmFromFlags ?? llmFromToml);
  maybeLock("selectedSttModels", sttFromFlags ?? sttFromToml);
  maybeLock("selectedImageModels", imageFromFlags ?? imageFromToml);
  maybeLock("activeLlmModel", flags.activeLlm ?? rawToml.activeLlmModel);
  maybeLock("activeSttModel", flags.activeStt ?? rawToml.activeSttModel);
  maybeLock("activeImageModel", flags.activeImage ?? rawToml.activeImageModel);
  maybeLock("hfToken", flags.hfToken ?? rawToml.hfToken);
  maybeLock("otelEndpoint", flags.otelEndpoint ?? rawToml.otelEndpoint);
  maybeLock("otelHeaders", flags.otelHeaders ?? rawToml.otelHeaders);
  maybeLock(
    "otelSampleRatio",
    flags.otelSampleRatio ?? rawToml.otelSampleRatio,
  );
  maybeLock("memory", rawToml.memory);

  const memory = memorySafetyConfigSchema.parse({
    systemReserve: {
      ...config.memory.systemReserve,
      ...rawToml.memory?.systemReserve,
    },
    acceleratorReserve: {
      ...config.memory.acceleratorReserve,
      ...rawToml.memory?.acceleratorReserve,
    },
  });

  config = {
    ...config,
    root,
    host: flags.host ?? rawToml.host ?? config.host,
    port: flags.port ?? rawToml.port ?? config.port,
    ctxSize: flags.ctxSize ?? rawToml.ctxSize ?? config.ctxSize,
    parallel,
    sttHost: flags.sttHost ?? rawToml.sttHost ?? config.sttHost,
    sttPort: flags.sttPort ?? rawToml.sttPort ?? config.sttPort,
    selectedLlmModels,
    selectedSttModels,
    selectedImageModels,
    activeLlmModel,
    activeSttModel,
    activeImageModel,
    hfToken:
      flags.hfToken ??
      rawToml.hfToken ??
      config.hfToken ??
      process.env.HF_TOKEN ??
      "",
    otelEndpoint:
      flags.otelEndpoint ?? rawToml.otelEndpoint ?? config.otelEndpoint,
    otelHeaders: flags.otelHeaders ?? rawToml.otelHeaders ?? config.otelHeaders,
    otelSampleRatio:
      flags.otelSampleRatio ??
      rawToml.otelSampleRatio ??
      config.otelSampleRatio,
    memory,
  };

  config = { ...config, ...modelDirectories(config.root) };

  const explicitMode =
    flags.all ||
    flags.defaults ||
    flags.configPath !== undefined ||
    locked.size > 0;
  if (
    execution.global.nonInteractive &&
    !hasConfig &&
    !flags.defaults &&
    !flags.configPath
  ) {
    throw new CliInputError(
      "Initial non-interactive configuration requires --defaults or --config.",
    );
  }
  const shouldAsk =
    !execution.global.nonInteractive &&
    (flags.all || (!flags.defaults && (!hasConfig || !explicitMode)));
  if (shouldAsk)
    config = await interactiveConfigureSelective(
      config,
      locked,
      specs.gpuVramGb,
    );

  validateComposedModelConfiguration(config);

  warnAboutParallelOomRisk(config.parallel, specs.gpuVramGb);

  saveConfig(ctx.database, config);
  execution.output.info(`Saved configuration to ${config.root}/local-base.db`);
  execution.output.info(
    `Selected LLM models: ${config.selectedLlmModels.join(", ")}`,
  );
  execution.output.info(
    `Selected STT models: ${config.selectedSttModels.join(", ")}`,
  );
  execution.output.info(
    `Selected Image models: ${config.selectedImageModels.join(", ")}`,
  );

  const hasAnyKeys = loadApiKeys(ctx.database, config).some(
    (k) => !k.revokedAt,
  );
  let createFirstKey = flags.createKey ?? !execution.global.json;
  if (flags.createKey === undefined && shouldAsk && !hasAnyKeys) {
    createFirstKey = await confirmPrompt(
      "No API keys found. Create one now",
      true,
    );
  }

  let createdKey:
    (ReturnType<typeof publicApiKey> & { secret: string }) | undefined;
  if (!hasAnyKeys && createFirstKey) {
    const { record, rawKey } = createApiKey(ctx.database, config, "default");
    execution.output.info("\nCreated initial API key:");
    execution.output.info(
      `id=${record.id} name=${record.name} prefix=${record.prefix}`,
    );
    if (!execution.global.json) {
      execution.output.info(`secret=${rawKey}`);
      execution.output.info("Store this secret now. It is not shown again.");
    }
    createdKey = { ...publicApiKey(record), secret: rawKey };
  }

  return {
    data: {
      configuration: publicConfiguration(
        config,
        resolveOtelConfiguration(config, process.env),
      ),
      ...(createdKey ? { createdKey } : {}),
    },
  };
}
