import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { loadTomlOverrides } from "./toml";

async function withToml(
  contents: string,
  action: (path: string) => Promise<void>,
): Promise<void> {
  const root = mkdtempSync(join(tmpdir(), "local-base-toml-"));
  const path = join(root, "local-base.toml");
  await Bun.write(path, contents);
  try {
    await action(path);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("normalizes legacy TOML model lists and validates typed overrides", async () => {
  await withToml(
    'port = 2273\nparallel = "auto"\nllmModels = ["qwen"]\nimageModels = ["flux"]\n',
    async (path) => {
      await expect(loadTomlOverrides(path)).resolves.toEqual({
        port: 2273,
        parallel: "auto",
        selectedLlmModels: ["qwen"],
        selectedImageModels: ["flux"],
      });
    },
  );
});

test("rejects TOML values with invalid configuration types", async () => {
  await withToml('port = "not-a-port"\n', async (path) => {
    await expect(loadTomlOverrides(path)).rejects.toThrow(/port/i);
  });
});
