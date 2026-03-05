import { installModel, type LocalBaseConfig } from "../../../manager";

export function runInstall(args: string[], config: LocalBaseConfig): number {
  const modelId = args[1];
  if (!modelId) {
    console.error("install requires <model_id>");
    return 2;
  }
  const path = installModel(config, modelId);
  console.log(`Installed: ${path}`);
  return 0;
}
