import { expect, test } from "bun:test";
import { modelsSchema, type Message } from "./client";
import {
  acceptsFirstFrame,
  canEditImages,
  imageRequestMode,
  imageResultLabel,
  imageResultText,
  sizeForAspect,
  studioResults,
} from "./studio";

function model(catalog: Record<string, unknown>) {
  return modelsSchema.parse({
    host: {
      memory: {
        kind: "unified",
        system: {
          capacityBytes: 16 * 1024 ** 3,
          availableBytes: 8 * 1024 ** 3,
        },
        accelerators: [],
      },
    },
    data: [
      {
        id: "media",
        catalog: {
          name: "Media",
          kind: "image",
          quantization: "Q8",
          qualificationState: "qualified",
          memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
          inputModalities: ["text"],
          outputModalities: ["image"],
          contextWindowTokens: null,
          capabilities: null,
          ...catalog,
        },
        device: { selected: true, installed: true, runtime: null },
      },
    ],
  }).data[0];
}
const video = (mode: string, inputModalities = ["text"]) =>
  model({
    kind: "video",
    inputModalities,
    outputModalities: ["video"],
    capabilities: {
      kind: "video",
      mode,
      width: 832,
      height: 480,
      frames: 81,
      fps: 16,
      jobDeadlineMs: 600000,
      outputFormats: ["mp4"],
    },
  });

test("only image-to-image models accept a source image", () => {
  expect(canEditImages(model({ features: ["image-to-image"] }))).toBe(true);
  expect(canEditImages(model({}))).toBe(false);
  expect(canEditImages(undefined)).toBe(false);
});

test("text-image-to-video models accept a first frame", () => {
  expect(acceptsFirstFrame(video("ti2v"))).toBe(true);
  expect(acceptsFirstFrame(video("t2v"))).toBe(false);
  expect(acceptsFirstFrame(video("t2v", ["text", "image"]))).toBe(true);
  expect(acceptsFirstFrame(video("s2v", ["text", "image", "audio"]))).toBe(
    false,
  );
  expect(acceptsFirstFrame(model({ features: ["image-to-image"] }))).toBe(
    false,
  );
  expect(
    acceptsFirstFrame(
      model({
        ...video("t2v").catalog,
        capabilityResolution: {
          capabilities: [],
          inputModalities: { values: ["text", "image"] },
        },
      }),
    ),
  ).toBe(true);
});

test("sources map to the nearest supported aspect", () => {
  expect(sizeForAspect(1920, 1080)).toBe("768x512");
  expect(sizeForAspect(1080, 1920)).toBe("512x768");
  expect(sizeForAspect(1000, 1100)).toBe("512x512");
});

test("results come from complete artifacts with their prompt and source", () => {
  const messages: Message[] = [
    { id: "u1", role: "user", text: "a fox" },
    {
      id: "a1",
      role: "assistant",
      text: "Generated 2 images",
      source: "data:image/png;base64,src",
      artifacts: [
        {
          id: "i1",
          label: "Image 1",
          state: "complete",
          media: { kind: "image", url: "data:image/png;base64,one" },
        },
        { id: "i2", label: "Image 2", state: "error", detail: "failed" },
      ],
    },
    { id: "u2", role: "user", text: "a boat" },
    {
      id: "a2",
      role: "assistant",
      text: "",
      artifacts: [{ id: "v1", label: "Video", state: "working", detail: "" }],
    },
  ];
  expect(studioResults(messages)).toEqual([
    {
      id: "i1",
      label: "Image 1",
      media: { kind: "image", url: "data:image/png;base64,one" },
      prompt: "a fox",
      source: "data:image/png;base64,src",
    },
  ]);
});

test("image results are labelled by the mode that produced them", () => {
  const editor = model({ features: ["image-to-image"] });
  const source = new Blob(["png"]);
  expect(imageRequestMode(undefined, editor)).toBe("text-to-image");
  expect(imageRequestMode({ source }, editor)).toBe("image-to-image");
  expect(imageRequestMode({ source, mask: source }, editor)).toBe("inpaint");
  expect(imageRequestMode({ source }, model({}))).toBe("text-to-image");
  expect(imageResultText("text-to-image", 1)).toBe("Generated image");
  expect(imageResultText("image-to-image", 1)).toBe("Transformed image");
  expect(imageResultText("inpaint", 2)).toBe("Inpainted 2 images");
  expect(imageResultLabel("inpaint", 0, 1)).toBe("Inpaint");
  expect(imageResultLabel("image-to-image", 1, 3)).toBe("Image to image 2");
});
