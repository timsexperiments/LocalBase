// The helpers that regenerate the Bun notices the Kokoro runtime packages (scripts/release-notices:
// bun-<version>.txt and native/), with the generate-*-notices.ts scripts. These cases came from
// release-notices.test.ts, which went with the TypeScript release.
import { expect, test } from "bun:test";
import { packageClosure, type Lock } from "./bun-lock-closure";
import { readmeLicenseSection } from "./package-readme-license";
import { leadingLegalCommentBlocks } from "./leading-legal-comment-blocks";

test("fallback closure follows nested versions and their distinct dependencies", () => {
  const lock = {
    workspaces: {
      "": { dependencies: { stream: "*", "readable-stream": "^4" } },
    },
    packages: {
      stream: [
        "stream@1.0.0",
        "",
        { dependencies: { "readable-stream": "^2" } },
        "root-integrity",
      ],
      "readable-stream": [
        "readable-stream@4.7.0",
        "",
        { dependencies: { "safe-buffer": "^5" } },
        "v4-integrity",
      ],
      "stream/readable-stream": [
        "readable-stream@2.3.8",
        "",
        { dependencies: { "process-nextick-args": "^2" } },
        "v2-integrity",
      ],
      "safe-buffer": ["safe-buffer@5.2.1", "", {}, "safe-integrity"],
      "stream/readable-stream/process-nextick-args": [
        "process-nextick-args@2.0.1",
        "",
        {},
        "nested-integrity",
      ],
    },
  } satisfies Lock;
  expect(packageClosure(lock).map((record) => record[0])).toEqual([
    "process-nextick-args@2.0.1",
    "readable-stream@2.3.8",
    "readable-stream@4.7.0",
    "safe-buffer@5.2.1",
    "stream@1.0.0",
  ]);
});

test("README license extraction retains the complete license section verbatim", () => {
  const license =
    "#### LICENSE\n\nCopyright Fedor Indutny, 2014.\n\nMIT permission text.\n\n";
  expect(readmeLicenseSection(`Intro\n\n${license}## References\nlink\n`)).toBe(
    license,
  );
  expect(readmeLicenseSection("No license section here.")).toBeUndefined();
});

test("leading legal comment collection continues after revision identifiers", () => {
  expect(
    leadingLegalCommentBlocks(
      "/* $NetBSD: pack_dev.c,v 1.12 $ */\n\n/* Copyright (c) 1998 The NetBSD Foundation. Redistribution is permitted. */\n\n#include <stdio.h>",
    ),
  ).toEqual([
    "/* Copyright (c) 1998 The NetBSD Foundation. Redistribution is permitted. */",
  ]);
});
