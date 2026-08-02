import { expect, test } from "bun:test";
import { LOCALBASE_VERSION } from "../src/version";
import { validateReleasePreflight } from "./release-preflight";

test("accepts the exact release tag and package version", () => {
  expect(() =>
    validateReleasePreflight(`v${LOCALBASE_VERSION}`, LOCALBASE_VERSION),
  ).not.toThrow();
});

test("rejects a tag that does not match the release version", () => {
  expect(() => validateReleasePreflight("v9.9.9", LOCALBASE_VERSION)).toThrow(
    `Release tag must be v${LOCALBASE_VERSION}`,
  );
});

test("rejects a package version that does not match the release version", () => {
  expect(() =>
    validateReleasePreflight(`v${LOCALBASE_VERSION}`, "9.9.9"),
  ).toThrow(`package.json version must be ${LOCALBASE_VERSION}`);
});
