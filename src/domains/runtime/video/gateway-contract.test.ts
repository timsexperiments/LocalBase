import { expect, test } from "bun:test";
import { byId } from "../../../catalog";
import { qualifiedVideoInput } from "./gateway-contract";

const model = byId("wan2.1-t2v-1.3b-q8_0");
if (!model) throw new Error("Expected qualified Wan video model.");

test("projects the qualified catalog profile into every native generation field", () => {
  expect(
    qualifiedVideoInput(
      {
        model: model.modelId,
        prompt: "A paper kite over a field.",
        width: 320,
        height: 320,
        frames: 33,
        fps: 16,
      },
      model,
    ),
  ).toEqual({
    prompt: "A paper kite over a field.",
    width: 320,
    height: 320,
    videoFrames: 33,
    fps: 16,
    seed: 42,
    outputFormat: "avi",
    generation: {
      sampler: "euler",
      scheduler: "default",
      steps: 20,
      cfgScale: 6,
      flowShift: 3,
    },
  });
});

test("does not construct an unqualified native video request", () => {
  expect(
    qualifiedVideoInput(
      {
        model: model.modelId,
        prompt: "A paper kite over a field.",
        width: 320,
        height: 304,
        frames: 33,
        fps: 16,
      },
      model,
    ),
  ).toBeUndefined();
});
