import { expect, test } from "bun:test";
import { join } from "node:path";
import {
  validateWorkflowDirectory,
  validateWorkflowSource,
} from "./github-actions-policy";

const workflow = (uses: string) => `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: ${uses}
`;

test("accepts the repository's pinned GitHub Actions workflows", async () => {
  expect(
    await validateWorkflowDirectory(
      join(import.meta.dir, "..", ".github", "workflows"),
    ),
  ).toEqual([]);
});

test("configures bounded individual GitHub Actions updates", async () => {
  expect(
    Bun.YAML.parse(
      await Bun.file(
        join(import.meta.dir, "..", ".github", "dependabot.yml"),
      ).text(),
    ),
  ).toEqual({
    version: 2,
    updates: [
      {
        "package-ecosystem": "github-actions",
        directory: "/",
        schedule: { interval: "weekly" },
        "open-pull-requests-limit": 3,
        allow: [
          { "dependency-name": "actions/checkout" },
          { "dependency-name": "actions/upload-artifact" },
          { "dependency-name": "actions/download-artifact" },
          { "dependency-name": "actions/attest-build-provenance" },
          { "dependency-name": "oven-sh/setup-bun" },
          { "dependency-name": "softprops/action-gh-release" },
        ],
      },
    ],
  });
});

test("rejects external actions outside the allowlist", () => {
  expect(
    validateWorkflowSource(
      "fixture.yml",
      workflow("someone/action@" + "a".repeat(40)),
    ),
  ).toEqual([
    "fixture.yml:5: action someone/action@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa is not allowlisted",
  ]);
});

test("requires immutable pins and matching major-version comments", () => {
  expect(
    validateWorkflowSource("fixture.yml", workflow("actions/checkout@v4 # v4")),
  ).toEqual([
    "fixture.yml:5: action actions/checkout must use a 40-character lowercase SHA",
  ]);
  expect(
    validateWorkflowSource(
      "fixture.yml",
      workflow(`actions/checkout@${"a".repeat(40)} # v3`),
    ),
  ).toEqual(["fixture.yml:5: action actions/checkout must be annotated # v4"]);
});

test("allows local composite actions without an external pin", () => {
  expect(
    validateWorkflowSource("fixture.yml", workflow("./.github/actions/check")),
  ).toEqual([]);
});
