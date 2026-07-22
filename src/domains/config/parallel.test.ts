import { describe, expect, test } from "bun:test";
import {
  allocateParallelSlots,
  parseOptionalParallelSlots,
  parseParallelSlots,
} from "./parallel";

describe("parallel slot configuration", () => {
  test("parses supported values without coercing invalid input", () => {
    expect(parseParallelSlots("auto")).toBe("auto");
    expect(parseParallelSlots(" AUTO ")).toBe("auto");
    expect(parseParallelSlots(" 2 ")).toBe(2);
    expect(parseOptionalParallelSlots(undefined)).toBeUndefined();

    for (const value of [0, 5, 1.5, "01", "1.0", "2x", "five"]) {
      expect(() => parseParallelSlots(value)).toThrow(
        'Use "auto" or an integer from 1 to 4',
      );
    }
  });

  test("honors a validated manual slot count", () => {
    expect(
      allocateParallelSlots({
        parallel: 3,
        memoryGb: 0,
        modelRequirementGb: 0,
        ctxSize: 8192,
      }),
    ).toEqual({ slots: 3, isAuto: false, contextPerSlot: 2730 });

    expect(() =>
      allocateParallelSlots({
        parallel: 4,
        memoryGb: 48,
        modelRequirementGb: 6,
        ctxSize: 4096,
      }),
    ).toThrow("Parallel slots 4 require at least 8192 total context tokens");
  });

  test("bounds auto slots by memory and total context", () => {
    expect(
      allocateParallelSlots({
        parallel: "auto",
        memoryGb: 48,
        modelRequirementGb: 6,
        ctxSize: 32768,
      }).slots,
    ).toBe(4);
    expect(
      allocateParallelSlots({
        parallel: "auto",
        memoryGb: 48,
        modelRequirementGb: 6,
        ctxSize: 4096,
      }).slots,
    ).toBe(2);
    expect(
      allocateParallelSlots({
        parallel: "auto",
        memoryGb: 9.5,
        modelRequirementGb: 6,
        ctxSize: 16384,
      }).slots,
    ).toBe(1);
  });
});
