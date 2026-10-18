import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { readdir } from "node:fs/promises";
import { join } from "node:path";
import { readLogSnapshot } from "../observability/logging";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
  TTS_MODEL,
  type GatewayFixture,
} from "../../test/gateway-fixture";
import { toneWav } from "../../test/voice-audio-fixture";
import { customVoicePath, customVoicesDirectory } from "./voice-store";

registerGatewayFixtureCleanup();

const adminKey = "lb_voice_admin_environment_key";
const voiceName = "Grandpa Story Voice";

describe("custom voice management", () => {
  let gateway: GatewayFixture;
  let userKey: string;

  beforeAll(
    async () => {
      gateway = await startGatewayFixture({
        auth: { mode: "bearer" },
        ttsEnabled: true,
        environmentApiKey: adminKey,
      });
      if (!gateway.apiKey) throw new Error("Expected fixture API key.");
      userKey = gateway.apiKey;
    },
    { timeout: 30_000 },
  );

  afterAll(
    async () => {
      await gateway?.stop();
    },
    { timeout: 10_000 },
  );

  const voices = (path = "", init: RequestInit = {}, key = adminKey) =>
    fetch(`${gateway.baseUrl}/_localbase/voices${path}`, {
      ...init,
      headers: { authorization: `Bearer ${key}`, ...init.headers },
    });

  function upload(
    fields: Record<string, string>,
    audio: Uint8Array | null = toneWav({ seconds: 3.5 }),
    filename = "clip.wav",
  ): FormData {
    const form = new FormData();
    for (const [key, value] of Object.entries(fields)) form.set(key, value);
    if (audio) form.set("audio", new File([new Uint8Array(audio)], filename));
    return form;
  }

  const create = (form: FormData, key = adminKey) =>
    voices("", { method: "POST", body: form }, key);

  const speak = (voice: string) =>
    fetch(`${gateway.baseUrl}/v1/audio/speech`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${userKey}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        model: TTS_MODEL,
        input: "Hello from a custom voice.",
        voice,
      }),
    });

  async function errorCode(response: Response) {
    const body = (await response.json()) as { error: { code: string } };
    return body.error.code;
  }

  test("lists for model readers and limits changes to model managers", async () => {
    expect((await fetch(`${gateway.baseUrl}/_localbase/voices`)).status).toBe(
      401,
    );
    const listed = await voices("", {}, userKey);
    expect(listed.status).toBe(200);
    expect(await listed.json()).toMatchObject({
      object: "list",
      data: [],
      canManage: false,
      limits: { minSeconds: 3, maxSeconds: 30 },
    });
    const denied = await create(
      upload({ name: "Mine", consent: "true" }),
      userKey,
    );
    expect(denied.status).toBe(403);
    expect(await errorCode(denied)).toBe("insufficient_permissions");
    expect(
      (await voices("/voice-0123456789ab", { method: "DELETE" }, userKey))
        .status,
    ).toBe(403);
    expect(
      (await voices("/voice-0123456789ab/audio", {}, userKey)).status,
    ).toBe(403);
    expect(await (await voices()).json().then((body) => body.canManage)).toBe(
      true,
    );
  });

  test("requires consent, a usable name and a bounded WAV", async () => {
    const cases: Array<[FormData, number, string]> = [
      [upload({ name: "Mine" }), 400, "consent_required"],
      [upload({ name: "Mine", consent: "false" }), 400, "consent_required"],
      [upload({ name: "Mine", consent: "on" }), 400, "consent_required"],
      [upload({ name: "Harbor", consent: "true" }), 400, "invalid_voice_name"],
      [upload({ name: "default", consent: "true" }), 400, "invalid_voice_name"],
      [upload({ consent: "true" }), 400, "invalid_voice_name"],
      [
        upload({ name: "Mine", consent: "true" }, null),
        400,
        "unsupported_audio_format",
      ],
      [
        upload(
          { name: "Mine", consent: "true" },
          new TextEncoder().encode("ID3 not really an mp3"),
        ),
        400,
        "unsupported_audio_format",
      ],
      [
        upload({ name: "Mine", consent: "true" }, toneWav({ seconds: 1 })),
        400,
        "audio_too_short",
      ],
      [
        upload(
          { name: "Mine", consent: "true" },
          toneWav({ seconds: 31, channels: 1, sampleRate: 8_000 }),
        ),
        400,
        "audio_too_long",
      ],
      [
        upload({ name: "Mine", consent: "true" }, new Uint8Array(13 << 20)),
        413,
        "audio_too_large",
      ],
    ];
    for (const [form, status, code] of cases) {
      const response = await create(form);
      expect(response.status).toBe(status);
      expect(await errorCode(response)).toBe(code);
    }
    const json = await voices("", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Mine", consent: true }),
    });
    expect(json.status).toBe(415);
    expect((await (await voices()).json()).data).toEqual([]);
    expect(
      await readdir(customVoicesDirectory(gateway.root)).catch(() => []),
    ).toEqual([]);
  });

  test("routes only well-formed voice ids", async () => {
    for (const path of [
      "/..%2F..%2Flocal-base.db",
      "/../../local-base.db",
      "/voice-0123456789ab/../../x",
      "/voice-0123456789ab/audio/extra",
      "/VOICE-0123456789AB",
      "/harbor",
    ]) {
      const response = await voices(path);
      expect(response.status).toBe(404);
    }
    const missing = await voices("/voice-0123456789ab");
    expect(missing.status).toBe(404);
    expect(await errorCode(missing)).toBe("voice_not_found");
  });

  test("creates, speaks with, renames and deletes a custom voice", async () => {
    // Warm the runtime first, so the new voice has to reload it.
    expect((await speak("default")).status).toBe(200);
    expect((await gateway.waitForTtsRuntimeEvent("started")).voice).toBe(
      "default",
    );

    const created = await create(
      upload(
        { name: voiceName, consent: "true" },
        toneWav({ seconds: 3.5 }),
        "../../evil.wav",
      ),
    );
    expect(created.status).toBe(201);
    const voice = (await created.json()) as {
      id: string;
      name: string;
      createdBy: string;
      sha256: string;
    };
    expect(voice).toMatchObject({ name: voiceName, createdBy: "environment" });
    expect(voice.id).toMatch(/^voice-[0-9a-f]{12}$/);
    const path = customVoicePath(gateway.root, voice.id);
    expect(await readdir(customVoicesDirectory(gateway.root))).toEqual([
      `${voice.id}.wav`,
    ]);
    expect(await Bun.file(join(gateway.root, "evil.wav")).exists()).toBe(false);

    const metadata = await fetch(
      `${gateway.baseUrl}/_localbase/models/${TTS_MODEL}`,
      { headers: { authorization: `Bearer ${userKey}` } },
    ).then((response) => response.json());
    expect(metadata.catalog.capabilities.voice).toMatchObject({
      requestValues: ["default", "harbor", "willow", voice.id],
      custom: [{ requestValue: voice.id, name: voiceName }],
    });
    const readerView = await voices("", {}, userKey).then((r) => r.json());
    expect(readerView.data[0]).not.toHaveProperty("createdBy");

    const preview = await voices(`/${voice.id}/audio`);
    expect(preview.headers.get("content-type")).toBe("audio/wav");
    expect(new Uint8Array(await preview.arrayBuffer())).toEqual(
      new Uint8Array(await Bun.file(path).arrayBuffer()),
    );

    const spoken = await speak(voice.id);
    expect(spoken.status).toBe(200);
    await spoken.arrayBuffer();
    // The warm server registered voices at launch, so it was replaced.
    await gateway.waitForTtsRuntimeEvent("stopped");
    const started = await gateway.waitForTtsRuntimeEvent("started");
    expect(started.voice).toBe(voice.id);
    expect(started.args).toEqual(
      expect.arrayContaining(["--voice", `${voice.id}=${path}`]),
    );

    const renamed = await voices(`/${voice.id}`, {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Story time" }),
    });
    expect(renamed.status).toBe(200);
    expect((await renamed.json()).name).toBe("Story time");
    const reserved = await voices(`/${voice.id}`, {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "WILLOW" }),
    });
    expect(reserved.status).toBe(400);
    expect(await errorCode(reserved)).toBe("invalid_voice_name");

    const deleted = await voices(`/${voice.id}`, { method: "DELETE" });
    expect(await deleted.json()).toEqual({
      id: voice.id,
      object: "localbase.voice",
      deleted: true,
    });
    expect(await Bun.file(path).exists()).toBe(false);
    const after = await speak(voice.id);
    expect(after.status).toBe(400);
    expect(await after.json()).toMatchObject({
      error: { code: "voice_not_found", param: "voice" },
    });
    expect((await voices(`/${voice.id}`)).status).toBe(404);
    // Ids that were never created fail the same way.
    expect((await speak("voice-ffffffffffff")).status).toBe(400);

    const events = await readLogSnapshot(gateway.root);
    const changes = events.filter(({ eventName }) =>
      eventName.startsWith("voices."),
    );
    expect(changes.map(({ eventName }) => eventName)).toEqual([
      "voices.created",
      "voices.renamed",
      "voices.deleted",
    ]);
    const logged = JSON.stringify(events);
    expect(logged).toContain(voice.id);
    expect(logged).not.toContain(voiceName);
    expect(logged).not.toContain("Story time");
    // HTTP routes are logged as templates, not as voice ids.
    expect(logged).toContain("/_localbase/voices/{voice_id}");
  });
});
