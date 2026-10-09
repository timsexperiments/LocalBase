import { expect, test } from "bun:test";
import { byId } from "../../catalog";
import { modelConfigurationSchema } from "./model-selection";
import {
  modelEligibilityReason,
  unsupportedModelTargetReason,
  experimentalModelOptInRequired,
} from "./model-eligibility";
import { defaultConfig } from "../../manager";

const id = "wan2.2-s2v-14b-fp8";
const linuxNvidia = {
  platform: "linux",
  architecture: "x64",
  accelerator: "nvidia",
} as const;

test("experimental S2V selection requires a persisted opt-in", () => {
  const config = defaultConfig("/tmp/localbase-model-selection-test");
  const selection = {
    allowExperimental: false,
    selectedLlmModels: config.selectedLlmModels,
    selectedSttModels: config.selectedSttModels,
    selectedTtsModels: config.selectedTtsModels,
    selectedImageModels: config.selectedImageModels,
    selectedVideoModels: [id],
    activeLlmModel: config.activeLlmModel,
    activeSttModel: config.activeSttModel,
    activeTtsModel: config.activeTtsModel,
    activeImageModel: config.activeImageModel,
    activeVideoModel: id,
  };
  expect(modelConfigurationSchema.safeParse(selection).success).toBe(false);
  expect(
    modelConfigurationSchema.safeParse({
      ...selection,
      allowExperimental: true,
    }).success,
  ).toBe(false);
  expect(
    modelEligibilityReason(byId(id)!, {
      allowExperimental: true,
      target: linuxNvidia,
    }),
  ).toBeNull();
});

test("S2V target eligibility rejects macOS ARM64 with the install reason", () => {
  expect(
    modelEligibilityReason(byId(id)!, {
      allowExperimental: true,
      platform: "darwin",
      architecture: "arm64",
    }),
  ).toBe(unsupportedModelTargetReason);
});

test("experimental model eligibility gives the opt-in reason before target checks", () => {
  expect(
    modelEligibilityReason(byId(id)!, {
      allowExperimental: false,
      platform: "darwin",
      architecture: "arm64",
    }),
  ).toBe(experimentalModelOptInRequired);
});
