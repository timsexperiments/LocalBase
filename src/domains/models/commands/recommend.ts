import { recommendedByKind, recommendedForVram, recommendedSttForVram } from "../../../catalog";
import { detectSpecs } from "../../../system";
import { parseFlag, parseKind, toInt } from "../../../utils/args";

export function runRecommend(args: string[]): number {
  const kind = parseKind(parseFlag(args, "--kind")) ?? "llm";
  const vram = toInt(parseFlag(args, "--vram"), detectSpecs().gpuVramGb);
  const picks = kind === "llm" ? recommendedForVram(vram) : kind === "stt" ? recommendedSttForVram(vram) : recommendedByKind(kind, vram);
  console.log(`Recommended ${kind.toUpperCase()} models for <= ${vram}GB VRAM:`);
  for (const model of picks.slice(0, 6)) {
    console.log(`- ${model.modelId} (${model.storageGb.toFixed(2)} GB, features=${model.features.join("/")})`);
  }
  return 0;
}
