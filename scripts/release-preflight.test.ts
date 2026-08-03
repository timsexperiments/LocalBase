import { expect, test } from "bun:test";
import { LOCALBASE_VERSION } from "../src/version";
import {
  validateReleasePreflight,
  validateRuntimeReleaseTag,
} from "./release-preflight";

test("accepts canonical Whisper runtime release tags", () => {
  for (const tag of [
    "whisper-v0.0.0",
    "whisper-v1.2.3",
    "whisper-v10.20.300",
  ]) {
    expect(() => validateRuntimeReleaseTag(tag)).not.toThrow();
  }
});

test("rejects non-canonical Whisper runtime release tags", () => {
  for (const tag of [
    "v1.2.3",
    "whisper-v01.2.3",
    "whisper-v1.02.3",
    "whisper-v1.2.03",
    "whisper-v1.2",
    "whisper-v1.2.3-rc.1",
    "whisper-v1.2.3+build",
    "whisper-v1.-2.3",
  ]) {
    expect(() => validateRuntimeReleaseTag(tag)).toThrow(
      "Runtime tag must use whisper-v<major>.<minor>.<patch>",
    );
  }
});

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
