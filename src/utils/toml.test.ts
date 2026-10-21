import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { loadTomlOverrides } from "./toml";
import { CliInputError } from "../domains/app/commands/errors";

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

test("parses standard TOML arrays and inline comments", async () => {
  await withToml(
    'host = "127.0.0.1" # gateway bind address\nport = 2273 # gateway port\nparallel = "auto"\nselectedLlmModels = [\n  "qwen3.8-27b-q4_k_m",\n]\nselectedImageModels = ["juggernaut-xl-v9"]\n[memory.systemReserve]\npercent = 20\n',
    async (path) => {
      await expect(loadTomlOverrides(path)).resolves.toEqual({
        host: "127.0.0.1",
        port: 2273,
        parallel: "auto",
        selectedLlmModels: ["qwen3.8-27b-q4_k_m"],
        selectedImageModels: ["juggernaut-xl-v9"],
        memory: {
          systemReserve: { percent: 20 },
        },
      });
    },
  );
});

test("accepts structurally valid existing video model selections", async () => {
  await withToml(
    'selectedVideoModels = ["fastwan2.2-ti2v-5b-q6_k"]\nactiveVideoModel = "fastwan2.2-ti2v-5b-q6_k"\n',
    async (path) => {
      await expect(loadTomlOverrides(path)).resolves.toMatchObject({
        selectedVideoModels: ["fastwan2.2-ti2v-5b-q6_k"],
        activeVideoModel: "fastwan2.2-ti2v-5b-q6_k",
      });
    },
  );
});

test("rejects malformed TOML", async () => {
  await withToml("selectedLlmModels = [\n", async (path) => {
    await expect(loadTomlOverrides(path)).rejects.toBeInstanceOf(CliInputError);
  });
});

test("rejects unknown fields and invalid shared configuration values", async () => {
  for (const contents of [
    "unsupported = true\n",
    'host = "not a host"\n',
    "port = 65536\n",
    "[memory.systemReserve]\npercent = -1\nminimumGb = 1\n",
  ]) {
    await withToml(contents, async (path) => {
      await expect(loadTomlOverrides(path)).rejects.toBeInstanceOf(
        CliInputError,
      );
    });
  }
});
