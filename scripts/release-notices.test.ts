import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import {
  generateReleaseNotices,
  productionDependencyClosure,
} from "./release-notices";

const temporaryDirectories: string[] = [];
function temp() {
  const directory = mkdtempSync("/tmp/localbase-release-notices-");
  temporaryDirectories.push(directory);
  return directory;
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
  } as const;
  expect(productionDependencyClosure(lock)).toEqual(["app", "nested"]);
  expect(() =>
    productionDependencyClosure({
      workspaces: { "": { dependencies: { absent: "1" } } },
      packages: {},
    }),
  ).toThrow("no production package record for absent");
});

test("notices contain full production package license texts and the pinned Bun notice", async () => {
  const notices = await generateReleaseNotices();
  expect(notices).toContain("## react@19.3.0");
  expect(notices).toContain("## zod@4.4.3");
  expect(notices).toContain("Permission is hereby granted, free of charge");
  expect(notices).toContain("## Bun 1.3.14");
  expect(notices).toContain("5488984d20e0dbfe4be2c3ba8fb18eb81a5e0e8b");
  expect(await generateReleaseNotices()).toBe(notices);
});

test("generation fails when a production package has no license text", async () => {
  const directory = temp();
  mkdirSync(join(directory, "node_modules", "no-license"), { recursive: true });
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
