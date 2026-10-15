import { expect, test } from "bun:test";
import {
  describeFit,
  describeLaunchShape,
  formatFileSize,
  formatGb,
  formatTokens,
} from "./model-details";

test("launch shapes are described in words", () => {
  expect(describeLaunchShape("default")).toBe("standard settings");
  expect(describeLaunchShape("ctx=8192;slots=1;kv=f16/f16")).toBe(
    "8,192-token context, 1 request at a time, f16 cache",
  );
  expect(describeLaunchShape("ctx=32768;slots=4;kv=q8_0/f16")).toBe(
    "32,768-token context, 4 requests at a time, q8_0/f16 cache",
  );
  expect(describeLaunchShape("something-new")).toBe("something-new");
});

test("sizes, tokens and fit read naturally", () => {
  expect(formatGb(1.5 * 1024 ** 3)).toBe("1.5 GB");
  expect(formatFileSize(null)).toBe("Unknown size");
  expect(formatFileSize(300)).toBe("1 KB");
  expect(formatFileSize(5 * 1024 ** 2)).toBe("5.0 MB");
  expect(formatTokens(32_768)).toBe("32,768 tokens");
  expect(
    describeFit({ verdict: "fits", systemMemoryGb: 32, headroomGb: 12.54 }),
  ).toBe("Fits, 12.5 GB to spare of 32 GB");
  expect(
    describeFit({ verdict: "wont-fit", systemMemoryGb: 8, headroomGb: -3 }),
  ).toBe("Won't fit, 3 GB short of 8 GB");
});
