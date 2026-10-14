import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { LlmKvGeometry } from "./gguf-metadata";
import {
  createLlmKvGeometryReader,
  createLlmTrainingContextLengthReader,
} from "./gguf-geometry-cache";

function geometry(blockCount: number): LlmKvGeometry {
  return {
    architecture: "qwen2",
    blockCount,
    fullKvHeads: 2,
    swaKvHeads: 0,
    slidingWindow: null,
    keyLength: 128,
    valueLength: 128,
    swaKeyLength: 128,
    swaValueLength: 128,
    q8Compatible: true,
    recurrentBytesPerSlot: 0,
    contextLength: 32768,
  };
}

test("retries null geometry and invalidates cached geometry after file changes", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-gguf-cache-"));
  try {
    const modelPath = join(root, "selected-model.gguf");
    const reads: Array<LlmKvGeometry | null> = [
      null,
      geometry(28),
      geometry(32),
      geometry(36),
    ];
    let readCount = 0;
    const readGeometry = createLlmKvGeometryReader(async () => {
      readCount += 1;
      return reads.shift() ?? null;
    });

    expect(await readGeometry(modelPath)).toBeNull();
    expect(readCount).toBe(0);

    writeFileSync(modelPath, "invalid header");
    expect(await readGeometry(modelPath)).toBeNull();
    expect(readCount).toBe(1);

    expect(await readGeometry(modelPath)).toEqual(geometry(28));
    expect(readCount).toBe(2);
    expect(await readGeometry(modelPath)).toEqual(geometry(28));
    expect(readCount).toBe(2);

    writeFileSync(modelPath, "installed GGUF model bytes");
    expect(await readGeometry(modelPath)).toEqual(geometry(32));
    expect(await readGeometry(modelPath)).toEqual(geometry(32));
    expect(readCount).toBe(3);

    writeFileSync(modelPath, "updated installed GGUF model bytes");
    expect(await readGeometry(modelPath)).toEqual(geometry(36));
    expect(readCount).toBe(4);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("caches the training context length until the metadata shard changes", async () => {
  const root = mkdtempSync(join(tmpdir(), "local-base-gguf-ctx-cache-"));
  try {
    const firstShard = join(root, "split-model-00001-of-00002.gguf");
    const secondShard = join(root, "split-model-00002-of-00002.gguf");
    writeFileSync(firstShard, "header shard");
    writeFileSync(secondShard, "tensor shard");
    const reads: string[] = [];
    const readContext = createLlmTrainingContextLengthReader(async (path) => {
      reads.push(path);
      return 4096 * reads.length;
    });

    expect(await readContext(secondShard)).toBe(4096);
    expect(await readContext(secondShard)).toBe(4096);
    expect(reads).toEqual([secondShard]);

    writeFileSync(secondShard, "changed tensor shard bytes");
    expect(await readContext(secondShard)).toBe(4096);
    expect(reads).toHaveLength(1);

    writeFileSync(firstShard, "changed header shard bytes");
    expect(await readContext(secondShard)).toBe(8192);
    expect(reads).toHaveLength(2);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
