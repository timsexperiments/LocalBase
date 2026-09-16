import { expect, test } from "bun:test";
import { byId } from "../../../catalog";
import { qualifiedVideoInput } from "./gateway-contract";

const model = byId("wan2.1-t2v-1.3b-q8_0");
if (!model) throw new Error("Expected qualified Wan video model.");

test.each([
  {
    id: "wan2.1-t2v-1.3b-q8_0",
    width: 320,
    height: 320,
    frames: 33,
    scheduler: "discrete",
    steps: 20,
    cfgScale: 6,
  },
  {
    id: "fastwan2.2-ti2v-5b-q6_k",
    width: 480,
    height: 832,
    frames: 81,
    scheduler: "lcm",
    steps: 3,
    cfgScale: 1,
  },
])(
  "projects $id into native generation fields",
  ({ id, width, height, frames, scheduler, steps, cfgScale }) => {
    const model = byId(id);
    if (!model) throw new Error("Expected qualified video model.");
    expect(
      qualifiedVideoInput(
        {
          model: model.modelId,
          prompt: "A paper kite over a field.",
          width,
          height,
          frames,
          fps: 16,
        },
        model,
      ),
    ).toEqual({
      prompt: "A paper kite over a field.",
      width,
      height,
      videoFrames: frames,
      fps: 16,
      seed: 42,
      outputFormat: "avi",
      generation: {
        sampler: "euler",
        scheduler,
        steps,
        cfgScale,
        flowShift: 3,
      },
    });
  },
);

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
