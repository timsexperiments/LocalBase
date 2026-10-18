import { afterAll, describe, expect, test } from "bun:test";
import {
  mkdtempSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import { toneWav } from "../../test/voice-audio-fixture";
import {
  CUSTOM_VOICE_LIMIT,
  VOICE_CONSENT_STATEMENT_ID,
  VoiceError,
} from "./voice-contract";
import {
  createCustomVoiceStore,
  customVoicePath,
  customVoicesDirectory,
} from "./voice-store";

const session = new DatabaseSession();
const parent = mkdtempSync(join(tmpdir(), "local-base-voice-store-"));
afterAll(() => {
  session.close();
  rmSync(parent, { recursive: true, force: true });
});

function fixture() {
  const root = mkdtempSync(join(parent, "root-"));
  return { root, store: createCustomVoiceStore(session.get(root), root) };
}

const clip = toneWav({ seconds: 3.5 });
const input = (overrides: Record<string, unknown> = {}) => ({
  name: "Narrator",
  audio: clip,
  consent: true,
  createdBy: "api-key:test",
  ...overrides,
});

async function code(promise: Promise<unknown> | (() => unknown)) {
  try {
    await (typeof promise === "function" ? promise() : promise);
  } catch (error) {
    if (error instanceof VoiceError) return error.code;
    throw error;
  }
  throw new Error("Expected a VoiceError.");
}

describe("custom voice store", () => {
  test("stores a normalized clip privately with its attestation", async () => {
    const { root, store } = fixture();
    const voice = await store.create(input({ name: "  My   Narrator " }));
    expect(voice.id).toMatch(/^voice-[0-9a-f]{12}$/);
    expect(voice).toMatchObject({
      name: "My Narrator",
      durationMs: 3_500,
      sizeBytes: 44 + 3.5 * 24_000 * 2,
      createdBy: "api-key:test",
      consent: { statement: VOICE_CONSENT_STATEMENT_ID },
    });
    expect(voice.consent.attestedAt).toBe(voice.createdAt);
    const path = customVoicePath(root, voice.id);
    const bytes = new Uint8Array(await Bun.file(path).arrayBuffer());
    expect(new Bun.CryptoHasher("sha256").update(bytes).digest("hex")).toBe(
      voice.sha256,
    );
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(statSync(customVoicesDirectory(root)).mode & 0o777).toBe(0o700);
    expect(store.list()).toEqual([voice]);
    expect(store.get(voice.id)).toEqual(voice);
    expect(await store.resolve(voice.id)).toBe(path);
    expect(await store.referenceFiles()).toEqual({ [voice.id]: path });
  });

  test("requires literal consent before reading the audio", async () => {
    const { store } = fixture();
    expect(await code(store.create(input({ consent: false })))).toBe(
      "consent_required",
    );
    // Consent is checked first, so even unreadable audio reports it.
    expect(
      await code(
        store.create(input({ consent: false, audio: new Uint8Array(4) })),
      ),
    ).toBe("consent_required");
    expect(store.list()).toEqual([]);
  });

  test("keeps built-in names reserved and names unique ignoring case", async () => {
    const { store } = fixture();
    for (const name of [
      "default",
      "Harbor",
      "WILLOW",
      "af_heart",
      "af_bella",
    ]) {
      expect(await code(store.create(input({ name })))).toBe(
        "invalid_voice_name",
      );
    }
    for (const name of [
      "",
      "   ",
      "a\u0000b",
      "x".repeat(49),
      "voice-0123456789ab",
    ]) {
      expect(await code(store.create(input({ name })))).toBe(
        "invalid_voice_name",
      );
    }
    const first = await store.create(input({ name: "Story" }));
    expect(await code(store.create(input({ name: "STORY" })))).toBe(
      "voice_name_taken",
    );
    const second = await store.create(input({ name: "Other" }));
    expect(await code(() => store.rename(second.id, "story"))).toBe(
      "voice_name_taken",
    );
    expect(await code(() => store.rename(second.id, "Harbor"))).toBe(
      "invalid_voice_name",
    );
    expect(store.rename(first.id, "story").name).toBe("story");
    expect(store.get(first.id)?.name).toBe("story");
  });

  test("rejects invalid audio without leaving files behind", async () => {
    const { root, store } = fixture();
    expect(
      await code(store.create(input({ audio: toneWav({ seconds: 1 }) }))),
    ).toBe("audio_too_short");
    expect(
      await code(
        store.create(input({ audio: new TextEncoder().encode("<?php") })),
      ),
    ).toBe("unsupported_audio_format");
    expect(store.list()).toEqual([]);
    const listing = await Array.fromAsync(
      new Bun.Glob("*").scan({ cwd: root, onlyFiles: false }),
    );
    expect(listing).not.toContain("voices");
  });

  test("never builds paths from anything but a voice id", async () => {
    const { root, store } = fixture();
    for (const id of [
      "../local-base.db",
      "voice-../../x",
      "VOICE-0123456789AB",
    ]) {
      expect(() => customVoicePath(root, id)).toThrow();
      expect(store.get(id)).toBeUndefined();
      expect(await store.resolve(id)).toBeUndefined();
      expect(await code(store.remove(id))).toBe("voice_not_found");
      expect(await code(() => store.rename(id, "x"))).toBe("voice_not_found");
    }
  });

  test("refuses a symlinked voice directory", async () => {
    const { root, store } = fixture();
    const elsewhere = mkdtempSync(join(parent, "elsewhere-"));
    symlinkSync(elsewhere, customVoicesDirectory(root));
    await expect(store.create(input())).rejects.toThrow("not a directory");
    expect(store.list()).toEqual([]);
  });

  test("deletes the row and the file", async () => {
    const { root, store } = fixture();
    const voice = await store.create(input());
    const path = customVoicePath(root, voice.id);
    expect((await store.remove(voice.id)).id).toBe(voice.id);
    expect(await Bun.file(path).exists()).toBe(false);
    expect(store.get(voice.id)).toBeUndefined();
    expect(await store.resolve(voice.id)).toBeUndefined();
    expect(await store.referenceFiles()).toEqual({});
    expect(await code(store.remove(voice.id))).toBe("voice_not_found");
  });

  test("skips references whose file no longer matches its checksum", async () => {
    const { root, store } = fixture();
    const kept = await store.create(input({ name: "Kept" }));
    const changed = await store.create(input({ name: "Changed" }));
    writeFileSync(customVoicePath(root, changed.id), toneWav({ seconds: 4 }));
    expect(Object.keys(await store.referenceFiles())).toEqual([kept.id]);
    expect(await store.resolve(changed.id)).toBeUndefined();
  });

  test("caps the number of stored voices", async () => {
    const { store } = fixture();
    for (let index = 0; index < CUSTOM_VOICE_LIMIT; index++) {
      await store.create(input({ name: `Voice ${index}` }));
    }
    expect(await code(store.create(input({ name: "One more" })))).toBe(
      "voice_limit_reached",
    );
  });
});
