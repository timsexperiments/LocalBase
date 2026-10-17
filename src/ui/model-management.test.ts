import { expect, test } from "bun:test";
import { storageSummary } from "./model-management";

test("storage shows free space, a pending check, or a failed one", () => {
  expect(
    storageSummary(
      { availableBytes: 2 * 1024 ** 3, totalBytes: 8 * 1024 ** 3 },
      false,
    ),
  ).toBe("2.0 GiB free of 8.0 GiB");
  expect(storageSummary(null, false)).toBe("Checking storage…");
  // A failed status poll must not leave the line checking forever.
  expect(storageSummary(null, true)).toBe("Storage unavailable");
});
