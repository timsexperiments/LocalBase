import {
  recommendedForVram,
  recommendedSttForVram,
  recommendedImageForVram,
} from "../../../catalog";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { RecommendInput } from "../../app/commands/inputs";

export function runRecommend(
  input: RecommendInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  const kind = input.kind ?? "llm";
  const vram = input.vram ?? ctx.specs.gpuVramGb;

  let picks = [];
  if (kind === "llm") {
    picks = recommendedForVram(vram);
  } else if (kind === "stt") {
    picks = recommendedSttForVram(vram);
  } else {
    picks = recommendedImageForVram(vram);
  }

  execution.output.info(
    `Recommended ${kind.toUpperCase()} models for <= ${vram}GB VRAM:`,
  );
  for (const model of picks.slice(0, 6)) {
    execution.output.info(
      `- ${model.modelId} (${model.storageGb.toFixed(2)} GB, features=${model.features.join("/")})`,
    );
  }
  return 0;
}
