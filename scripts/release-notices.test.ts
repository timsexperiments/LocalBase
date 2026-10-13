import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import {
  generateReleaseNotices,
  nativeNoticeManifest,
  productionDependencyClosure,
} from "./release-notices";
import { packageClosure, type Lock } from "./bun-lock-closure";
import { readmeLicenseSection } from "./package-readme-license";
import { leadingLegalCommentBlocks } from "./leading-legal-comment-blocks";

const temporaryDirectories: string[] = [];
function temp() {
  const directory = mkdtempSync("/tmp/localbase-release-notices-");
  temporaryDirectories.push(directory);
  return directory;
}

async function writeNativeFiles(directory: string, contents = "license") {
  mkdirSync(join(directory, "scripts", "release-notices", "native"), {
    recursive: true,
  });
  for (const filename of new Set(Object.values(nativeNoticeManifest).flat()))
    await Bun.write(
      join(directory, "scripts/release-notices/native", filename),
      contents,
    );
}

afterEach(() =>
  temporaryDirectories
    .splice(0)
    .forEach((directory) =>
      rmSync(directory, { recursive: true, force: true }),
    ),
);

test("production closure follows only root production dependencies and their dependencies", () => {
  const lock = {
    workspaces: { "": { dependencies: { app: "1" } } },
    packages: {
      app: ["app@1", "", { dependencies: { nested: "1" } }],
      nested: ["nested@1", "", {}],
      devOnly: ["devOnly@1", "", {}],
    },
  } satisfies Lock;
  expect(productionDependencyClosure(lock)).toEqual(["app", "nested"]);
  expect(() =>
    productionDependencyClosure({
      workspaces: { "": { dependencies: { absent: "1" } } },
      packages: {},
    }),
  ).toThrow("no production package record for absent");
});

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

test("notices contain full production package license texts and the pinned Bun notice", async () => {
  const notices = await generateReleaseNotices();
  expect(notices).toContain("## react@19.3.0");
  expect(notices).toContain("## zod@4.4.3");
  expect(notices).toContain("Permission is hereby granted, free of charge");
  expect(notices).toContain("## Bun 1.3.14");
  expect(notices).toContain("5488984d20e0dbfe4be2c3ba8fb18eb81a5e0e8b");
  expect(notices).toContain("base64-js@1.5.1");
  expect(notices).toContain("pako@1.0.11");
  for (const packageName of [
    "asn1.js@4.10.1",
    "brorand@1.1.0",
    "des.js@1.1.0",
    "elliptic@6.6.1",
    "hash.js@1.1.7",
    "hmac-drbg@1.0.1",
    "miller-rabin@4.0.1",
    "minimalistic-crypto-utils@1.0.1",
  ]) {
    expect(notices).toContain(`===== ${packageName} `);
    expect(notices).toContain("Copyright Fedor Indutny,");
  }
  expect(notices).toContain("Copyright (c) 2003-2010 Tim Kientzle");
  expect(notices).toContain("Copyright (c) 2008-2009 Bjoern Hoehrmann");
  expect(notices).toContain(
    "Copyright (c) 1998, 2001 The NetBSD Foundation, Inc.",
  );
  expect(notices).toContain("by Charles M. Hannum.");
  expect(notices).toContain("UNICODE LICENSE V3");
  expect(notices).toContain("Copyright © 2024 Unicode, Inc.");
  expect(notices).toContain(
    "Hoehrmann's available decoder license has been recovered",
  );
  expect(await generateReleaseNotices()).toBe(notices);
});

test("generation fails when a production package has no license text", async () => {
  const directory = temp();
  mkdirSync(join(directory, "node_modules", "no-license"), { recursive: true });
  await Bun.write(
    join(directory, "node_modules", "no-license", "NOTICE"),
    "notice only",
  );
  mkdirSync(join(directory, "scripts", "release-notices"), { recursive: true });
  mkdirSync(join(directory, "src", "manager"), { recursive: true });
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@1.3.14" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({
      workspaces: { "": { dependencies: { "no-license": "1.0.0" } } },
      packages: { "no-license": ["no-license@1.0.0", "", {}] },
    }),
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "bun-1.3.14.txt"),
    "Bun notice",
  );
  await Bun.write(
    join(directory, "src", "manager", "managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "No license text found for bundled dependency no-license",
  );
});

test("generation rejects an empty pinned Bun notice", async () => {
  const directory = temp();
  await writeNativeFiles(directory);
  mkdirSync(join(directory, "src", "manager"), { recursive: true });
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@1.3.14" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({ workspaces: { "": { dependencies: {} } }, packages: {} }),
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "bun-1.3.14.txt"),
    " \n ",
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "native", "Bun-MIT.txt"),
    "MIT text",
  );
  await Bun.write(
    join(directory, "src", "manager", "managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "Bun notices for pinned Bun 1.3.14 are empty",
  );
});

test("generation rejects a missing or empty vendored native license", async () => {
  const directory = temp();
  await writeNativeFiles(directory);
  mkdirSync(join(directory, "src", "manager"), { recursive: true });
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@1.3.14" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({ workspaces: { "": { dependencies: {} } }, packages: {} }),
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "bun-1.3.14.txt"),
    "Bun notice",
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "native", "boringssl.txt"),
    " \n ",
  );
  await Bun.write(
    join(directory, "src", "manager", "managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "Vendored native license file boringssl.txt is empty",
  );
});

test("generation rejects a missing required native license while others remain", async () => {
  const directory = temp();
  await writeNativeFiles(directory);
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@1.3.14" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({ workspaces: { "": { dependencies: {} } }, packages: {} }),
  );
  await Bun.write(
    join(directory, "scripts/release-notices/bun-1.3.14.txt"),
    "Bun notice",
  );
  await Bun.write(
    join(directory, "src/manager/managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  rmSync(join(directory, "scripts/release-notices/native/boringssl.txt"));
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "Missing required native license file boringssl.txt",
  );
});

test("generation rejects a missing vendored native license directory", async () => {
  const directory = temp();
  mkdirSync(join(directory, "scripts", "release-notices"), { recursive: true });
  mkdirSync(join(directory, "src", "manager"), { recursive: true });
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@1.3.14" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({ workspaces: { "": { dependencies: {} } }, packages: {} }),
  );
  await Bun.write(
    join(directory, "scripts", "release-notices", "bun-1.3.14.txt"),
    "Bun notice",
  );
  await Bun.write(
    join(directory, "src", "manager", "managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "No vendored native license texts found",
  );
});

test("generation fails when the pinned Bun version has no checked-in notice", async () => {
  const directory = temp();
  mkdirSync(join(directory, "node_modules"), { recursive: true });
  mkdirSync(join(directory, "scripts", "release-notices"), { recursive: true });
  mkdirSync(join(directory, "src", "manager"), { recursive: true });
  await Bun.write(
    join(directory, "package.json"),
    JSON.stringify({ packageManager: "bun@9.9.9" }),
  );
  await Bun.write(
    join(directory, "bun.lock"),
    JSON.stringify({ workspaces: { "": { dependencies: {} } }, packages: {} }),
  );
  await Bun.write(
    join(directory, "src", "manager", "managed-runtime-manifest.json"),
    JSON.stringify({ targets: [] }),
  );
  await expect(generateReleaseNotices(directory)).rejects.toThrow(
    "Missing versioned Bun notices for pinned Bun 9.9.9",
  );
});
