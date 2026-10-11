import { expect, test } from "bun:test";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
} from "../../test/gateway-fixture";
import { tinyPng } from "../../test/media-fixtures";
import { testPng } from "./video/video-input.fixtures";

registerGatewayFixtureCleanup();

test("accepts multipart image edits and returns the image response shape", async () => {
  const gateway = await startGatewayFixture();
  try {
    const form = new FormData();
    form.set("prompt", "A watercolor landscape");
    form.set("strength", "0.6");
    form.append(
      "image[]",
      new File([tinyPng], "input.png", { type: "image/png" }),
    );
    form.set("mask", new File([tinyPng], "mask.png", { type: "image/png" }));
    const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: form,
    });
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({
      data: [{ b64_json: expect.any(String) }],
    });
    const upstream = gateway.upstreamRequests.find(
      (item) => item.path === "/v1/images/edits",
    );
    expect(upstream?.body).toContain(
      '<sd_cpp_extra_args>{"strength":0.6}</sd_cpp_extra_args>',
    );
    expect(upstream?.body).toContain('name="image[]"');
    expect(upstream?.formData?.get("mask")).toBeInstanceOf(File);
    expect(upstream?.formData?.get("prompt")).toContain(
      '<sd_cpp_extra_args>{"strength":0.6}</sd_cpp_extra_args>',
    );
  } finally {
    await gateway.stop();
  }
});

test("rejects sd_cpp_extra_args injection and invalid image bytes", async () => {
  const gateway = await startGatewayFixture();
  try {
    const injected = new FormData();
    injected.set(
      "prompt",
      'safe <sd_cpp_extra_args>{"strength":1}</sd_cpp_extra_args>',
    );
    injected.append(
      "image",
      new File([tinyPng], "input.png", { type: "image/png" }),
    );
    const injectionResponse = await fetch(
      `${gateway.baseUrl}/v1/images/edits`,
      { method: "POST", body: injected },
    );
    expect(injectionResponse.status).toBe(400);

    const uppercase = new FormData();
    uppercase.set("prompt", "SAFE SD_CPP_EXTRA_ARGS marker");
    uppercase.append(
      "image",
      new File([tinyPng], "input.png", { type: "image/png" }),
    );
    const upperResponse = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: uppercase,
    });
    expect(upperResponse.status).toBe(400);

    const invalid = new FormData();
    invalid.set("prompt", "safe");
    invalid.append(
      "image",
      new File([new Uint8Array([1, 2, 3])], "input.png", { type: "image/png" }),
    );
    const invalidResponse = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: invalid,
    });
    expect(invalidResponse.status).toBe(400);
  } finally {
    await gateway.stop();
  }
});

test("rejects invalid image edit masks, dimensions, size, and response format before backend", async () => {
  const gateway = await startGatewayFixture();
  try {
    for (const [field, value, param] of [
      ["mask", new File([], "empty.png", { type: "image/png" }), "mask"],
      ["mask", "not-a-file", "mask"],
      ["response_format", "bogus", "response_format"],
      ["size", "16384x16384", "size"],
    ] as const) {
      const form = new FormData();
      form.set("prompt", "valid");
      form.append(
        "image",
        new File([tinyPng], "input.png", { type: "image/png" }),
      );
      form.set(field, value as string | File);
      const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
        method: "POST",
        body: form,
      });
      expect(response.status).toBe(400);
      expect(await response.json()).toMatchObject({ error: { param } });
    }
    const image = new Uint8Array(tinyPng);
    image[19] = 0x08;
    image[20] = 0x01;
    const oversized = new FormData();
    oversized.set("prompt", "valid");
    oversized.append(
      "image",
      new File([image], "huge.png", { type: "image/png" }),
    );
    const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: oversized,
    });
    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: { param: "image" } });
    expect(
      gateway.upstreamRequests.filter(
        (item) => item.path === "/v1/images/edits",
      ),
    ).toHaveLength(0);
  } finally {
    await gateway.stop();
  }
});

test("rejects incomplete and over-expanding image and mask PNGs before admission", async () => {
  const gateway = await startGatewayFixture();
  try {
    for (const [field, bytes] of [
      ["mask", tinyPng.subarray(0, 24)],
      ["image", tinyPng.subarray(0, 24)],
      [
        "image",
        Uint8Array.fromBase64(
          testPng({
            width: 2,
            height: 1,
            inflated: new Uint8Array(8 * 1024 * 1024),
          }),
        ),
      ],
    ] as const) {
      const form = new FormData();
      form.set("prompt", "valid");
      form.append(
        "image",
        new File([tinyPng], "input.png", { type: "image/png" }),
      );
      form.set(field, new File([bytes], `${field}.png`, { type: "image/png" }));
      const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
        method: "POST",
        body: form,
      });
      expect(response.status).toBe(400);
      expect(await response.json()).toMatchObject({ error: { param: field } });
    }
    const webp = new Uint8Array(30);
    webp.set(new TextEncoder().encode("RIFF"), 0);
    new DataView(webp.buffer).setUint32(4, 22, true);
    webp.set(new TextEncoder().encode("WEBPVP8X"), 8);
    const form = new FormData();
    form.set("prompt", "valid");
    form.append(
      "image",
      new File([webp], "input.webp", { type: "image/webp" }),
    );
    const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: form,
    });
    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: { param: "image" } });
    expect(
      gateway.upstreamRequests.filter(
        (item) => item.path === "/v1/images/edits",
      ),
    ).toHaveLength(0);
  } finally {
    await gateway.stop();
  }
});

test("accepts JPEG marker fill bytes and rejects a request above the edit limit with that limit", async () => {
  const gateway = await startGatewayFixture();
  try {
    // Insert a legal marker fill byte after SOI in this valid 1x1 JPEG.
    const jpegBytes = Uint8Array.from(
      atob(
        "/9j/4AAQSkZJRgABAgAAAQABAAD//gAPTGF2YzYzLjEuMTAyAP/bAEMACAQEBAQEBQUFBQUFBgYGBgYGBgYGBgYGBgcHBwgICAcHBwYGBwcICAgICQkJCAgICAkJCgoKDAwLCw4ODhERFP/EAEwAAQEAAAAAAAAAAAAAAAAAAAAGAQEBAAAAAAAAAAAAAAAAAAAGBxABAAAAAAAAAAAAAAAAAAAAABEBAAAAAAAAAAAAAAAAAAAAAP/AABEIAAEAAQMBIgACEQADEQD/2gAMAwEAAhEDEQA/AIsATX9//9k=",
      ),
      (character) => character.charCodeAt(0),
    );
    const jpeg = new Uint8Array(jpegBytes.byteLength + 1);
    jpeg.set(jpegBytes.subarray(0, 2));
    jpeg[2] = 0xff;
    jpeg.set(jpegBytes.subarray(2), 3);
    const form = new FormData();
    form.set("prompt", "valid");
    form.append("image", new File([jpeg], "input.jpg", { type: "image/jpeg" }));
    expect(
      (
        await fetch(`${gateway.baseUrl}/v1/images/edits`, {
          method: "POST",
          body: form,
        })
      ).status,
    ).toBe(200);

    const oversized = new FormData();
    oversized.set("prompt", "valid");
    oversized.append(
      "image",
      new File([new Uint8Array(20 * 1024 * 1024 + 1)], "large.png"),
    );
    const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: oversized,
    });
    expect(response.status).toBe(413);
    expect(await response.json()).toMatchObject({
      error: { message: expect.stringContaining("20 MiB limit") },
    });
  } finally {
    await gateway.stop();
  }
});

test("rejects a model without the image-to-image catalog feature", async () => {
  const gateway = await startGatewayFixture();
  try {
    const form = new FormData();
    form.set("model", "flux1-schnell-q4_0");
    form.set("prompt", "A watercolor landscape");
    form.append(
      "image",
      new File([tinyPng], "input.png", { type: "image/png" }),
    );
    const response = await fetch(`${gateway.baseUrl}/v1/images/edits`, {
      method: "POST",
      body: form,
    });
    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({
      error: { code: "validation_failed", param: "model" },
    });
  } finally {
    await gateway.stop();
  }
});
