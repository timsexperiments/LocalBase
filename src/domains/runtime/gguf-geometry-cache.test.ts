import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { LlmKvGeometry } from "./gguf-metadata";
import { createLlmKvGeometryReader } from "./gguf-geometry-cache";

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
