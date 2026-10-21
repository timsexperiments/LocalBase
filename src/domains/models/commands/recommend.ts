import { legacyFeatures, withLegacyFeatures } from "../../../catalog";
import type { ModelSpec, ModelSpecWithFeatures } from "../../../catalog";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { RecommendInput } from "../../app/commands/inputs";
import {
  evaluateModelFit,
  modelMemoryJson,
  recommendedForVram,
  recommendedImageForVram,
  recommendedSttForVram,
  recommendedTtsForVram,
  type ModelMemoryJson,
} from "../memory-fit";
import { cliMemoryInputs } from "./memory-inputs";

export function runRecommend(
  input: RecommendInput,
  ctx: AppContext,
  execution: CommandExecution,
): {
  data: {
    kind: "llm" | "stt" | "tts" | "image" | "video";
    vramGb: number;
    models: (ModelSpecWithFeatures & ModelMemoryJson)[];
  };
} {
  const kind = input.kind ?? "llm";
  const vram = input.vram ?? ctx.specs.gpuVramGb;

  let picks: ModelSpec[] = [];
  if (kind === "llm") {
    picks = recommendedForVram(vram);
  } else if (kind === "stt") {
    picks = recommendedSttForVram(vram);
  } else if (kind === "tts") {
    picks = recommendedTtsForVram(vram);
  } else if (kind === "image") {
    picks = recommendedImageForVram(vram);
  }

  execution.output.info(
    `Recommended ${kind.toUpperCase()} models for <= ${vram}GB VRAM:`,
  );
  const shown = picks.slice(0, 6);
  const memory = cliMemoryInputs(ctx, shown);
  const fits = shown.map((model) =>
    evaluateModelFit(model, vram, memory.get(model.modelId)),
  );
  shown.forEach((model, index) => {
    const fit = fits[index]!;
    execution.output.info(
      `- ${model.modelId} (${model.storageGb.toFixed(2)} GB, planning min ${fit.minVramGb} GB ${fit.source}, features=${legacyFeatures(model).join("/")})`,
    );
  });
  return {
    data: {
      kind,
      vramGb: vram,
      models: shown.map((model, index) => ({
        ...withLegacyFeatures(model),
        ...modelMemoryJson(fits[index]!),
      })),
    },
  };
}
