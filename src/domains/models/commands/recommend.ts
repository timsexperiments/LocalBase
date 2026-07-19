import {
  recommendedForVram,
  recommendedSttForVram,
  recommendedImageForVram,
} from "../../../catalog";
import { parseFlag, parseKind, toInt } from "../../../utils/args";
import type { AppContext } from "../../../context";

export function runRecommend(args: string[], ctx: AppContext): number {
  const kind = parseKind(parseFlag(args, "--kind")) ?? "llm";
  const vram = toInt(parseFlag(args, "--vram"), ctx.specs.gpuVramGb);

  let picks = [];
  if (kind === "llm") {
    picks = recommendedForVram(vram);
  } else if (kind === "stt") {
    picks = recommendedSttForVram(vram);
  } else {
    picks = recommendedImageForVram(vram);
  }

  console.log(
    `Recommended ${kind.toUpperCase()} models for <= ${vram}GB VRAM:`,
  );
  for (const model of picks.slice(0, 6)) {
    console.log(
      `- ${model.modelId} (${model.storageGb.toFixed(2)} GB, features=${model.features.join("/")})`,
    );
  }
  return 0;
}
