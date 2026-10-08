import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import {
  ggufMetadataPath,
  kvCacheBytes,
  kvGeometryFromMetadata,
  readLlmKvGeometry,
} from "./gguf-metadata";

const directory = mkdtempSync(join(tmpdir(), "local-base-gguf-"));
afterAll(() => rmSync(directory, { recursive: true, force: true }));

type Entry =
  | { key: string; type: "u32"; value: number }
  | { key: string; type: "string"; value: string }
  | { key: string; type: "u32[]"; value: number[] }
  | { key: string; type: "bool[]"; value: boolean[] }
  | { key: string; type: "string[]"; value: string[] };

function u32(value: number): Buffer {
  const out = Buffer.alloc(4);
  out.writeUInt32LE(value);
  return out;
}

function u64(value: number): Buffer {
  const out = Buffer.alloc(8);
  out.writeBigUInt64LE(BigInt(value));
  return out;
}

function str(value: string): Buffer {
  const bytes = Buffer.from(value);
  return Buffer.concat([u64(bytes.length), bytes]);
}

function ggufFile(entries: Entry[], magic = 0x46554747): Buffer {
  const parts = [u32(magic), u32(3), u64(0), u64(entries.length)];
  for (const entry of entries) {
    parts.push(str(entry.key));
    if (entry.type === "u32") parts.push(u32(4), u32(entry.value));
    else if (entry.type === "string") parts.push(u32(8), str(entry.value));
    else if (entry.type === "u32[]") {
      parts.push(
        u32(9),
        u32(4),
        u64(entry.value.length),
        ...entry.value.map(u32),
      );
    } else if (entry.type === "bool[]") {
      parts.push(
        u32(9),
        u32(7),
        u64(entry.value.length),
        Buffer.from(entry.value.map((flag) => (flag ? 1 : 0))),
      );
    } else {
      parts.push(
        u32(9),
        u32(8),
        u64(entry.value.length),
        ...entry.value.map(str),
      );
    }
  }
  return Buffer.concat(parts);
}

function write(name: string, entries: Entry[], magic?: number): string {
  const path = join(directory, name);
  writeFileSync(path, ggufFile(entries, magic));
  return path;
}

/** One recurrent layer: 4-byte (3 * (4096 + 2 * 16 * 128) + 128 * 4096). */
const RECURRENT_LAYER_BYTES = 4 * (3 * (4096 + 2 * 16 * 128) + 128 * 4096);

function ssm(arch: string): Entry[] {
  return [
    { key: `${arch}.ssm.conv_kernel`, type: "u32", value: 4 },
    { key: `${arch}.ssm.inner_size`, type: "u32", value: 4096 },
    { key: `${arch}.ssm.state_size`, type: "u32", value: 128 },
    { key: `${arch}.ssm.group_count`, type: "u32", value: 16 },
  ];
}

function base(arch: string, extra: Entry[]): Entry[] {
  const key = (suffix: string) => `${arch}.${suffix}`;
  return [
    { key: "general.architecture", type: "string", value: arch },
    {
      key: "tokenizer.ggml.tokens",
      type: "string[]",
      value: Array.from({ length: 2000 }, (_, i) => `tok${i}`.repeat(50)),
    },
    { key: key("block_count"), type: "u32", value: 40 },
    { key: key("attention.head_count"), type: "u32", value: 32 },
    ...extra,
  ];
}

describe("readLlmKvGeometry", () => {
  test("parses scalar KV heads and skips large string arrays", async () => {
    const path = write(
      "llama.gguf",
      base("llama", [
        { key: "llama.attention.head_count_kv", type: "u32", value: 8 },
        { key: "llama.attention.key_length", type: "u32", value: 128 },
        { key: "llama.attention.value_length", type: "u32", value: 128 },
      ]),
    );
    const geometry = await readLlmKvGeometry(path);
    expect(geometry).toMatchObject({
      blockCount: 40,
      fullKvHeads: 320,
      swaKvHeads: 0,
      keyLength: 128,
      valueLength: 128,
    });
    expect(
      kvCacheBytes(geometry!, {
        ctxTokens: 1,
        slots: 1,
        cacheTypeK: "f16",
        cacheTypeV: "f16",
      }),
    ).toBe(163840 * 256);
  });

  test("falls back to embedding_length / head_count", async () => {
    const path = write(
      "fallback.gguf",
      base("llama", [
        { key: "llama.embedding_length", type: "u32", value: 4096 },
      ]),
    );
    expect(await readLlmKvGeometry(path)).toMatchObject({
      keyLength: 128,
      valueLength: 128,
      fullKvHeads: 40 * 32,
    });
  });

  test("defaults value length independently of an explicit key length", async () => {
    const path = write(
      "value-fallback.gguf",
      base("llama", [
        { key: "llama.embedding_length", type: "u32", value: 5120 },
        { key: "llama.attention.key_length", type: "u32", value: 128 },
      ]),
    );
    expect(await readLlmKvGeometry(path)).toMatchObject({
      keyLength: 128,
      valueLength: 160,
    });
  });

  test("handles per-layer KV head arrays with zero-head layers", async () => {
    const heads = Array.from({ length: 40 }, (_, i) => (i % 2 ? 0 : 4));
    const path = write(
      "array.gguf",
      base("hybrid", [
        { key: "hybrid.attention.head_count_kv", type: "u32[]", value: heads },
        { key: "hybrid.attention.key_length", type: "u32", value: 64 },
      ]),
    );
    expect(await readLlmKvGeometry(path)).toMatchObject({
      fullKvHeads: 80,
      valueLength: 64,
    });
  });

  test("counts only full-attention layers for hybrid intervals", async () => {
    const path = write("qwen35.gguf", [
      { key: "general.architecture", type: "string", value: "qwen35" },
      { key: "qwen35.block_count", type: "u32", value: 33 },
      { key: "qwen35.attention.head_count", type: "u32", value: 16 },
      { key: "qwen35.attention.head_count_kv", type: "u32", value: 4 },
      { key: "qwen35.attention.key_length", type: "u32", value: 256 },
      { key: "qwen35.attention.value_length", type: "u32", value: 256 },
      { key: "qwen35.full_attention_interval", type: "u32", value: 4 },
      ...ssm("qwen35"),
    ]);
    const geometry = await readLlmKvGeometry(path);
    expect(geometry?.fullKvHeads).toBe(8 * 4);
    expect(
      kvCacheBytes(geometry!, {
        ctxTokens: 1,
        slots: 1,
        cacheTypeK: "f16",
        cacheTypeV: "f16",
      }),
    ).toBe(32768 * 256 + 25 * RECURRENT_LAYER_BYTES);
  });

  test("defaults qwen3next to a four-layer interval", async () => {
    const path = write("qwen3next.gguf", [
      { key: "general.architecture", type: "string", value: "qwen3next" },
      { key: "qwen3next.block_count", type: "u32", value: 48 },
      { key: "qwen3next.attention.head_count", type: "u32", value: 16 },
      { key: "qwen3next.attention.head_count_kv", type: "u32", value: 2 },
      { key: "qwen3next.attention.key_length", type: "u32", value: 256 },
      { key: "qwen3next.attention.value_length", type: "u32", value: 256 },
      ...ssm("qwen3next"),
    ]);
    expect((await readLlmKvGeometry(path))?.fullKvHeads).toBe(12 * 2);
  });

  test("models gpt-oss sliding-window layers as a per-slot term", async () => {
    const path = write("gpt-oss.gguf", [
      { key: "general.architecture", type: "string", value: "gpt-oss" },
      { key: "gpt-oss.block_count", type: "u32", value: 24 },
      { key: "gpt-oss.attention.head_count", type: "u32", value: 64 },
      { key: "gpt-oss.attention.head_count_kv", type: "u32", value: 8 },
      { key: "gpt-oss.attention.key_length", type: "u32", value: 64 },
      { key: "gpt-oss.attention.value_length", type: "u32", value: 64 },
      { key: "gpt-oss.attention.sliding_window", type: "u32", value: 128 },
    ]);
    const geometry = await readLlmKvGeometry(path);
    expect(geometry).toMatchObject({
      fullKvHeads: 12 * 8,
      swaKvHeads: 12 * 8,
      slidingWindow: 128,
    });
    const perHead = 64 * 2 * 2;
    expect(
      kvCacheBytes(geometry!, {
        ctxTokens: 32768,
        slots: 2,
        cacheTypeK: "f16",
        cacheTypeV: "f16",
      }),
    ).toBe(32768 * 96 * perHead + 2 * 768 * 96 * perHead);
  });

  describe("sliding-window architectures", () => {
    const gemma3 = (extra: Entry[] = []): Entry[] => [
      { key: "general.architecture", type: "string", value: "gemma3" },
      { key: "gemma3.block_count", type: "u32", value: 48 },
      { key: "gemma3.attention.head_count", type: "u32", value: 16 },
      { key: "gemma3.attention.head_count_kv", type: "u32", value: 8 },
      { key: "gemma3.attention.key_length", type: "u32", value: 256 },
      { key: "gemma3.attention.value_length", type: "u32", value: 256 },
      { key: "gemma3.attention.sliding_window", type: "u32", value: 1024 },
      ...extra,
    ];
    const f16 = (ctxTokens: number, slots = 1) => ({
      ctxTokens,
      slots,
      cacheTypeK: "q8_0" as const,
      cacheTypeV: "q8_0" as const,
    });

    test("sizes Gemma 3 12B with a 5:1 sliding-window pattern", async () => {
      const geometry = await readLlmKvGeometry(write("gemma3.gguf", gemma3()));
      expect(geometry).toMatchObject({
        fullKvHeads: 8 * 8,
        swaKvHeads: 40 * 8,
        slidingWindow: 1024,
      });
      const gib = kvCacheBytes(geometry!, f16(32768)) / 1024 ** 3;
      expect(gib).toBeCloseTo(1.3115, 3);
    });

    test("honours an explicit sliding_window_pattern", async () => {
      const geometry = await readLlmKvGeometry(
        write(
          "gemma3-pattern.gguf",
          gemma3([
            {
              key: "gemma3.attention.sliding_window_pattern",
              type: "u32",
              value: 4,
            },
          ]),
        ),
      );
      expect(geometry).toMatchObject({
        fullKvHeads: 12 * 8,
        swaKvHeads: 36 * 8,
      });
    });

    test("uses period 2 and a 4096 default window for Gemma 2", async () => {
      const geometry = await readLlmKvGeometry(
        write("gemma2.gguf", [
          { key: "general.architecture", type: "string", value: "gemma2" },
          { key: "gemma2.block_count", type: "u32", value: 26 },
          { key: "gemma2.attention.head_count", type: "u32", value: 8 },
          { key: "gemma2.attention.head_count_kv", type: "u32", value: 4 },
          { key: "gemma2.attention.key_length", type: "u32", value: 256 },
        ]),
      );
      expect(geometry).toMatchObject({
        fullKvHeads: 13 * 4,
        swaKvHeads: 13 * 4,
        slidingWindow: 4096,
      });
    });

    test("treats Gemma 3 without a window as full attention", async () => {
      const entries = gemma3().filter(
        (entry) => !entry.key.endsWith("sliding_window"),
      );
      const geometry = await readLlmKvGeometry(
        write("gemma3-full.gguf", entries),
      );
      expect(geometry).toMatchObject({
        fullKvHeads: 48 * 8,
        swaKvHeads: 0,
        slidingWindow: null,
      });
    });

    test("limits Gemma 3n to its 20 KV layers with period 5", async () => {
      const geometry = await readLlmKvGeometry(
        write("gemma3n.gguf", [
          { key: "general.architecture", type: "string", value: "gemma3n" },
          { key: "gemma3n.block_count", type: "u32", value: 30 },
          { key: "gemma3n.attention.head_count", type: "u32", value: 8 },
          { key: "gemma3n.attention.head_count_kv", type: "u32", value: 2 },
          { key: "gemma3n.attention.key_length", type: "u32", value: 256 },
          { key: "gemma3n.attention.sliding_window", type: "u32", value: 512 },
        ]),
      );
      expect(geometry).toMatchObject({
        fullKvHeads: 4 * 2,
        swaKvHeads: 16 * 2,
      });
    });
  });

  describe("MLA", () => {
    const deepseek = (extra: Entry[]): Entry[] => [
      { key: "general.architecture", type: "string", value: "deepseek2" },
      { key: "deepseek2.block_count", type: "u32", value: 27 },
      { key: "deepseek2.attention.head_count", type: "u32", value: 16 },
      { key: "deepseek2.attention.head_count_kv", type: "u32", value: 1 },
      { key: "deepseek2.attention.key_length", type: "u32", value: 576 },
      { key: "deepseek2.attention.value_length", type: "u32", value: 512 },
      ...extra,
    ];
    const f16 = {
      ctxTokens: 131072,
      slots: 1,
      cacheTypeK: "q8_0" as const,
      cacheTypeV: "q8_0" as const,
    };

    test("allocates only K when MLA lengths are present", async () => {
      const geometry = await readLlmKvGeometry(
        write(
          "mla.gguf",
          deepseek([
            {
              key: "deepseek2.attention.key_length_mla",
              type: "u32",
              value: 192,
            },
            {
              key: "deepseek2.attention.value_length_mla",
              type: "u32",
              value: 128,
            },
          ]),
        ),
      );
      expect(geometry?.valueLength).toBe(0);
      expect(kvCacheBytes(geometry!, f16) / 1024 ** 3).toBeCloseTo(2.0171, 3);
    });

    test("keeps legacy non-MLA sizing with both K and V", async () => {
      const geometry = await readLlmKvGeometry(
        write("legacy-mla.gguf", deepseek([])),
      );
      expect(geometry?.valueLength).toBe(512);
      expect(kvCacheBytes(geometry!, f16) / 1024 ** 3).toBeCloseTo(3.8101, 3);
    });
  });

  describe("hostile headers", () => {
    test("rejects a huge block_count without iterating", async () => {
      const path = write("hostile.gguf", [
        { key: "general.architecture", type: "string", value: "llama" },
        { key: "llama.block_count", type: "u32", value: 4294967295 },
        { key: "llama.attention.head_count", type: "u32", value: 32 },
        { key: "llama.embedding_length", type: "u32", value: 4096 },
      ]);
      const start = performance.now();
      expect(await readLlmKvGeometry(path)).toBeNull();
      expect(performance.now() - start).toBeLessThan(500);
    });

    test("rejects implausible heads, lengths, and per-layer entries", async () => {
      const cases: Entry[][] = [
        [{ key: "llama.attention.head_count_kv", type: "u32", value: 100000 }],
        [{ key: "llama.attention.key_length", type: "u32", value: 1 << 20 }],
        [
          {
            key: "llama.attention.head_count_kv",
            type: "u32[]",
            value: Array.from({ length: 40 }, (_, i) => (i === 7 ? 1e6 : 4)),
          },
        ],
      ];
      for (const [index, extra] of cases.entries()) {
        expect(
          await readLlmKvGeometry(
            write(`bad-${index}.gguf`, base("llama", extra)),
          ),
        ).toBeNull();
      }
    });

    test("treats u64 scalars beyond 2^53 as invalid", () => {
      const values = new Map<string, number | string>([
        ["general.architecture", "llama"],
        ["llama.block_count", Number.NaN],
        ["llama.attention.head_count", 32],
      ]);
      expect(kvGeometryFromMetadata(values as never)).toBeNull();
    });
  });

  describe("q8_0 compatibility", () => {
    const llama = (headLength: number, extra: Entry[] = []) =>
      base("llama", [
        { key: "llama.attention.head_count_kv", type: "u32", value: 8 },
        { key: "llama.attention.key_length", type: "u32", value: headLength },
        ...extra,
      ]);

    test("accepts head dims divisible by 32 and rejects others", async () => {
      expect(
        (await readLlmKvGeometry(write("q-128.gguf", llama(128))))
          ?.q8Compatible,
      ).toBe(true);
      expect(
        (await readLlmKvGeometry(write("q-80.gguf", llama(80))))?.q8Compatible,
      ).toBe(false);
    });

    test("checks V length independently of K", async () => {
      const geometry = await readLlmKvGeometry(
        write(
          "q-v.gguf",
          llama(128, [
            { key: "llama.attention.value_length", type: "u32", value: 100 },
          ]),
        ),
      );
      expect(geometry?.q8Compatible).toBe(false);
    });

    test("checks SWA dims and MLA dims", async () => {
      const gemma = (swaKey: number) => [
        {
          key: "general.architecture",
          type: "string" as const,
          value: "gemma3",
        },
        { key: "gemma3.block_count", type: "u32" as const, value: 12 },
        { key: "gemma3.attention.head_count", type: "u32" as const, value: 8 },
        {
          key: "gemma3.attention.key_length",
          type: "u32" as const,
          value: 256,
        },
        {
          key: "gemma3.attention.sliding_window",
          type: "u32" as const,
          value: 512,
        },
        {
          key: "gemma3.attention.key_length_swa",
          type: "u32" as const,
          value: swaKey,
        },
      ];
      expect(
        (await readLlmKvGeometry(write("q-swa-ok.gguf", gemma(256))))
          ?.q8Compatible,
      ).toBe(true);
      const mla = await readLlmKvGeometry(
        write("q-mla.gguf", [
          { key: "general.architecture", type: "string", value: "deepseek2" },
          { key: "deepseek2.block_count", type: "u32", value: 4 },
          { key: "deepseek2.attention.head_count", type: "u32", value: 16 },
          { key: "deepseek2.attention.key_length", type: "u32", value: 576 },
          { key: "deepseek2.attention.value_length", type: "u32", value: 512 },
          {
            key: "deepseek2.attention.key_length_mla",
            type: "u32",
            value: 192,
          },
          {
            key: "deepseek2.attention.value_length_mla",
            type: "u32",
            value: 100,
          },
        ]),
      );
      expect(mla?.valueLength).toBe(0);
      expect(mla?.q8Compatible).toBe(false);
    });
  });

  describe("Gemma 4", () => {
    const gemma4 = (extra: Entry[] = []): Entry[] => [
      { key: "general.architecture", type: "string", value: "gemma4" },
      { key: "gemma4.block_count", type: "u32", value: 35 },
      { key: "gemma4.attention.head_count", type: "u32", value: 8 },
      { key: "gemma4.attention.head_count_kv", type: "u32", value: 1 },
      { key: "gemma4.attention.key_length", type: "u32", value: 512 },
      { key: "gemma4.attention.value_length", type: "u32", value: 512 },
      { key: "gemma4.attention.key_length_swa", type: "u32", value: 256 },
      { key: "gemma4.attention.value_length_swa", type: "u32", value: 256 },
      { key: "gemma4.attention.sliding_window", type: "u32", value: 512 },
      { key: "gemma4.attention.shared_kv_layers", type: "u32", value: 15 },
      {
        key: "gemma4.attention.sliding_window_pattern",
        type: "bool[]",
        value: Array.from({ length: 35 }, (_, i) => (i + 1) % 5 !== 0),
      },
      ...extra,
    ];

    test("uses the per-layer pattern, shared-KV cutoff and SWA dims", async () => {
      const geometry = await readLlmKvGeometry(write("gemma4.gguf", gemma4()));
      expect(geometry).toMatchObject({
        fullKvHeads: 4,
        swaKvHeads: 16,
        swaKeyLength: 256,
        swaValueLength: 256,
        q8Compatible: true,
      });
      const gib =
        kvCacheBytes(geometry!, {
          ctxTokens: 32768,
          slots: 1,
          cacheTypeK: "q8_0",
          cacheTypeV: "q8_0",
        }) /
        1024 ** 3;
      expect(gib).toBeCloseTo(0.141, 3);
    });

    test("rejects a pattern whose length differs from block_count", async () => {
      const entries = gemma4().filter(
        (e) => !e.key.endsWith("sliding_window_pattern"),
      );
      entries.push({
        key: "gemma4.attention.sliding_window_pattern",
        type: "bool[]",
        value: [true, false],
      });
      expect(
        await readLlmKvGeometry(write("gemma4-bad.gguf", entries)),
      ).toBeNull();
    });
  });

  describe("hybrid recurrent models", () => {
    const qwen = (arch: string, extra: Entry[] = []): Entry[] => [
      { key: "general.architecture", type: "string", value: arch },
      { key: `${arch}.block_count`, type: "u32", value: 48 },
      { key: `${arch}.attention.head_count`, type: "u32", value: 16 },
      { key: `${arch}.attention.head_count_kv`, type: "u32", value: 2 },
      { key: `${arch}.attention.key_length`, type: "u32", value: 256 },
      ...ssm(arch),
      ...extra,
    ];

    test("adds FP32 conv/ssm state per slot (Qwen3-Next, 4 slots)", async () => {
      const geometry = await readLlmKvGeometry(
        write("qn.gguf", qwen("qwen3next")),
      );
      expect(geometry?.recurrentBytesPerSlot).toBe(36 * RECURRENT_LAYER_BYTES);
      expect(
        ((geometry?.recurrentBytesPerSlot ?? 0) * 4) / 1024 ** 2,
      ).toBeCloseTo(301.5, 1);
    });

    test("defaults the interval to 4 for qwen35 and qwen35moe", async () => {
      for (const arch of ["qwen35", "qwen35moe"]) {
        const geometry = await readLlmKvGeometry(
          write(`${arch}.gguf`, qwen(arch)),
        );
        expect(geometry?.fullKvHeads).toBe(12 * 2);
      }
    });

    test("prefers attention.recurrent_layers over the interval", async () => {
      const mask = Array.from({ length: 48 }, (_, i) => i >= 6);
      const geometry = await readLlmKvGeometry(
        write(
          "qn-mask.gguf",
          qwen("qwen3next", [
            {
              key: "qwen3next.attention.recurrent_layers",
              type: "bool[]",
              value: mask,
            },
            { key: "qwen3next.full_attention_interval", type: "u32", value: 4 },
          ]),
        ),
      );
      expect(geometry?.fullKvHeads).toBe(6 * 2);
      expect(geometry?.recurrentBytesPerSlot).toBe(42 * RECURRENT_LAYER_BYTES);
    });

    test("returns null for unmodelled recurrent architectures", async () => {
      const entries = qwen("mamba2");
      expect(await readLlmKvGeometry(write("mamba2.gguf", entries))).toBeNull();
    });
  });

  describe("per-layer head counts", () => {
    test("sizes OpenELM-270M from head_count and head_count_kv arrays", async () => {
      const heads = [
        12, 12, 12, 12, 12, 16, 16, 16, 16, 16, 16, 16, 20, 20, 20, 20,
      ];
      const kvHeads = [3, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5];
      const geometry = await readLlmKvGeometry(
        write("openelm.gguf", [
          { key: "general.architecture", type: "string", value: "openelm" },
          { key: "openelm.block_count", type: "u32", value: 16 },
          { key: "openelm.attention.head_count", type: "u32[]", value: heads },
          {
            key: "openelm.attention.head_count_kv",
            type: "u32[]",
            value: kvHeads,
          },
          { key: "openelm.attention.key_length", type: "u32", value: 64 },
        ]),
      );
      expect(geometry?.fullKvHeads).toBe(63);
      const gib =
        kvCacheBytes(geometry!, {
          ctxTokens: 32768,
          slots: 1,
          cacheTypeK: "q8_0",
          cacheTypeV: "q8_0",
        }) /
        1024 ** 3;
      expect(gib).toBeCloseTo(0.2615, 3);
    });

    test("uses head_count when head_count_kv is absent and rejects wrong lengths", async () => {
      const perLayer = await readLlmKvGeometry(
        write("hc.gguf", [
          { key: "general.architecture", type: "string", value: "openelm" },
          { key: "openelm.block_count", type: "u32", value: 4 },
          {
            key: "openelm.attention.head_count",
            type: "u32[]",
            value: [1, 2, 3, 4],
          },
          { key: "openelm.attention.key_length", type: "u32", value: 64 },
        ]),
      );
      expect(perLayer?.fullKvHeads).toBe(10);
      const short = await readLlmKvGeometry(
        write("hc-short.gguf", [
          { key: "general.architecture", type: "string", value: "openelm" },
          { key: "openelm.block_count", type: "u32", value: 4 },
          {
            key: "openelm.attention.head_count",
            type: "u32[]",
            value: [1, 2, 3],
          },
          { key: "openelm.attention.key_length", type: "u32", value: 64 },
        ]),
      );
      expect(short).toBeNull();
    });
  });

  describe("cell padding", () => {
    const geometry = {
      architecture: "llama",
      blockCount: 1,
      fullKvHeads: 1,
      swaKvHeads: 0,
      slidingWindow: null,
      keyLength: 1,
      valueLength: 0,
      swaKeyLength: 1,
      swaValueLength: 0,
      q8Compatible: false,
      recurrentBytesPerSlot: 0,
      contextLength: null,
    };
    const cells = (ctxTokens: number, slots: number) =>
      kvCacheBytes(geometry, {
        ctxTokens,
        slots,
        cacheTypeK: "f32",
        cacheTypeV: "f32",
      }) / 4;

    test("pads the context and each stream to 256 cells", () => {
      expect(cells(1000, 1)).toBe(1024);
      expect(cells(1000, 4)).toBe(1024);
      expect(cells(1000, 3)).toBe(3 * 512);
      expect(cells(1, 1)).toBe(256);
    });
  });

  describe("parse hardening", () => {
    function raw(...parts: Buffer[]): string {
      const path = join(
        directory,
        `raw-${Math.random().toString(36).slice(2)}.gguf`,
      );
      writeFileSync(
        path,
        Buffer.concat([u32(0x46554747), u32(3), u64(0), ...parts]),
      );
      return path;
    }
    const header = (count: number) => u64(count);
    const arrayHeader = (key: string, count: number) =>
      Buffer.concat([str(key), u32(9), u32(8), u64(count)]);

    test("rejects string arrays whose count exceeds the file", async () => {
      const path = raw(
        header(1),
        arrayHeader("tokenizer.ggml.tokens", 2 ** 40),
      );
      const start = performance.now();
      expect(await readLlmKvGeometry(path)).toBeNull();
      expect(performance.now() - start).toBeLessThan(500);
    });

    test("rejects a string skip that runs past EOF or overflows", async () => {
      for (const length of [1_000_000, 2 ** 53 - 1]) {
        const path = raw(
          header(1),
          arrayHeader("tokenizer.ggml.tokens", 1),
          u64(length),
          Buffer.alloc(16),
        );
        expect(await readLlmKvGeometry(path)).toBeNull();
      }
    });

    test("enforces a total parse-work budget", async () => {
      const entries = base("llama", [
        { key: "llama.attention.head_count_kv", type: "u32", value: 8 },
        { key: "llama.attention.key_length", type: "u32", value: 128 },
      ]);
      const path = write("budget.gguf", entries);
      expect(await readLlmKvGeometry(path)).not.toBeNull();
      expect(await readLlmKvGeometry(path, { maxParseWork: 100 })).toBeNull();
    });
  });

  test("resolves split shards to the first file", async () => {
    expect(ggufMetadataPath("/m/Model-Q4-00003-of-00004.gguf")).toBe(
      "/m/Model-Q4-00001-of-00004.gguf",
    );
    expect(ggufMetadataPath("/m/Model.gguf")).toBe("/m/Model.gguf");
    const first = write(
      "Split-00001-of-00002.gguf",
      base("llama", [
        { key: "llama.attention.head_count_kv", type: "u32", value: 8 },
        { key: "llama.attention.key_length", type: "u32", value: 128 },
      ]),
    );
    expect(first).toContain("00001");
    expect(
      await readLlmKvGeometry(join(directory, "Split-00002-of-00002.gguf")),
    ).not.toBeNull();
  });

  test("returns null for bad magic, missing file, and missing keys", async () => {
    expect(
      await readLlmKvGeometry(write("bad.gguf", base("llama", []), 0x1234)),
    ).toBeNull();
    expect(await readLlmKvGeometry(join(directory, "missing.gguf"))).toBeNull();
    expect(
      await readLlmKvGeometry(
        write("nokeys.gguf", [
          { key: "general.architecture", type: "string", value: "llama" },
        ]),
      ),
    ).toBeNull();
  });
});

describe("kvCacheBytes", () => {
  test("scales with cache element size", async () => {
    const geometry = {
      architecture: "llama",
      blockCount: 40,
      fullKvHeads: 320,
      swaKvHeads: 0,
      slidingWindow: null,
      keyLength: 128,
      valueLength: 128,
      swaKeyLength: 128,
      swaValueLength: 128,
      q8Compatible: true,
      recurrentBytesPerSlot: 0,
      contextLength: null,
    };
    const bytes = (type: "f16" | "q8_0" | "q4_0" | "f32") =>
      kvCacheBytes(geometry, {
        ctxTokens: 32768,
        slots: 1,
        cacheTypeK: type,
        cacheTypeV: type,
      });
    expect(bytes("f16")).toBe(5 * 1024 ** 3);
    expect(bytes("f32")).toBe(10 * 1024 ** 3);
    expect(bytes("q8_0")).toBe((5 * 1024 ** 3 * 34) / 64);
    expect(bytes("q4_0")).toBe((5 * 1024 ** 3 * 18) / 64);
  });
});

const realModel = join(
  homedir(),
  ".local/share/local-base/models/llm/Qwen_Qwen3.5-9B-Q4_K_M.gguf",
);

test.skipIf(!existsSync(realModel))(
  "reads the real Qwen3.5-9B header",
  async () => {
    const geometry = await readLlmKvGeometry(realModel);
    expect(geometry).toMatchObject({
      architecture: "qwen35",
      fullKvHeads: 32,
      keyLength: 256,
    });
  },
);
