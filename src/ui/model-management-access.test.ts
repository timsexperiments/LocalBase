import { expect, test } from "bun:test";
import { join } from "node:path";
import { startGatewayFixture } from "../test/gateway-fixture";
import { DatabaseSession } from "../db/client";
import { resolveApiKey, setApiKeyScopes } from "../manager";
import { defaultApiKeyScopes } from "../domains/auth/authorization";

test("authorized management HTTP validates bounded JSON and persists config mutations", async () => {
  const gateway = await startGatewayFixture({ auth: { mode: "bearer" } });
  let restarted: Bun.Subprocess | undefined;
  try {
    await gateway.stop({ preserveRoot: true });
    const key = gateway.apiKey;
    if (!key) throw new Error("Expected fixture API key.");
    const database = new DatabaseSession();
    let storedKey;
    try {
      storedKey = resolveApiKey(database, gateway.readConfig(), key);
      if (storedKey)
        setApiKeyScopes(database, gateway.readConfig(), storedKey.id, [
          ...defaultApiKeyScopes,
          "models:manage",
        ]);
    } finally {
      database.close();
    }
    if (!storedKey) throw new Error("Expected stored fixture key.");
    // Reuse the fixture's compiled fake runtimes and isolated config after startup authorization changes.
    restarted = Bun.spawn(
      [
        gateway.cliPath,
        "serve",
        "--root",
        gateway.root,
        "--host",
        "127.0.0.1",
        "--port",
        new URL(gateway.baseUrl).port,
        "--auth-mode",
        "bearer",
        "--bypass-memory-check",
      ],
      {
        env: {
          ...process.env,
          PATH: `${join(gateway.root, "test-runtimes")}:${process.env.PATH ?? ""}`,
        },
        stdout: "ignore",
        stderr: "ignore",
      },
    );
    const url = `${gateway.baseUrl}/_localbase/model-management`;
    const headers = {
      authorization: `Bearer ${key}`,
      "content-type": "application/json",
    };
    const deadline = Date.now() + 10_000;
    let ready = false;
    while (Date.now() < deadline) {
      if (restarted.exitCode !== null)
        throw new Error("Test gateway exited before listening.");
      const response = await fetch(url, {
        headers,
        signal: AbortSignal.timeout(500),
      }).catch(() => null);
      if (response) {
        expect(response.status).toBe(200);
        expect(await response.json()).toMatchObject({ canManage: true });
        ready = true;
        break;
      }
      await Bun.sleep(25);
    }
    expect(ready).toBe(true);
    const modelId = "whisper-large-v3-turbo";
    const before = gateway.readConfig();
    expect(before.selectedSttModels).toContain(modelId);
    const post = (body: BodyInit) =>
      fetch(url, { method: "POST", headers, body });
    for (const body of [
      "{",
      "null",
      "[]",
      "{}",
      JSON.stringify({ modelId, action: "erase" }),
      JSON.stringify({ modelId, action: "disable", extra: "private-token" }),
      JSON.stringify({ modelId: "../../private-token", action: "disable" }),
    ]) {
      const response = await post(body);
      expect(response.status).toBe(400);
      const text = await response.text();
      expect(text).not.toContain(gateway.root);
      expect(text).not.toContain("private-token");
    }
    const oversized = await post(" ".repeat(1025));
    expect(oversized.status).toBe(413);
    await oversized.body?.cancel();
    const streamed = await post(
      new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(" ".repeat(700)));
          controller.enqueue(new TextEncoder().encode(" ".repeat(700)));
          controller.close();
        },
      }),
    );
    expect(streamed.status).toBe(413);
    await streamed.body?.cancel();
    expect(gateway.readConfig().selectedSttModels).toEqual(
      before.selectedSttModels,
    );
    const conflict = await post(
      JSON.stringify({ modelId: before.activeLlmModel, action: "disable" }),
    );
    expect(conflict.status).toBe(409);
    expect(await conflict.json()).toMatchObject({
      error: {
        code: "conflict",
        message: "Activate another LLM before disabling the default.",
      },
    });
    const changed = await post(JSON.stringify({ modelId, action: "disable" }));
    expect(changed.status).toBe(200);
    expect(await changed.json()).toMatchObject({
      action: "disable",
      state: "complete",
    });
    expect(gateway.readConfig().selectedSttModels).not.toContain(modelId);
    expect(gateway.readConfig().activeSttModel).toBe("");
    const read = await fetch(url, { headers });
    expect(await read.json()).toMatchObject({
      canManage: true,
      models: expect.arrayContaining([
        expect.objectContaining({ id: modelId, enabled: false, active: false }),
      ]),
    });
  } finally {
    if (restarted) {
      restarted.kill("SIGTERM");
      const timeout = setTimeout(() => restarted?.kill("SIGKILL"), 5000);
      try {
        await restarted.exited;
      } finally {
        clearTimeout(timeout);
      }
    }
    await gateway.stop();
  }
}, 30_000);

test("management remains authenticated when inference auth is disabled", async () => {
  const gateway = await startGatewayFixture();
  try {
    for (const method of ["GET", "POST"]) {
      const response = await fetch(
        `${gateway.baseUrl}/_localbase/model-management`,
        {
          method,
          headers: {
            authorization: "Bearer arbitrary",
            "x-localbase-owner-id": `api-key:key_${crypto.randomUUID()}`,
            "cf-access-authenticated-user-email": "person@example.com",
          },
        },
      );
      expect(response.status).toBe(401);
      expect(await response.json()).toMatchObject({
        error: { code: "invalid_api_key" },
      });
    }
  } finally {
    await gateway.stop();
  }
}, 30_000);
