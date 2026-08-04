import { expect, test } from "bun:test";
import { join } from "node:path";
import { byId, primaryArtifact } from "../../catalog";
import {
  startGatewayFixture,
  type GatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";
import { gatewayHealthSchema } from "./health";

const STT_MODEL = "whisper-large-v3-turbo";
const IMAGE_MODEL = "stable-diffusion-v1-5";

async function imageRequest(gateway: GatewayFixture): Promise<Response> {
  return await fetch(`${gateway.baseUrl}/v1/images/generations`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ prompt: "fixture" }),
  });
}

async function transcriptionRequest(
  gateway: GatewayFixture,
): Promise<Response> {
  const body = new FormData();
  body.append("file", new Blob(["fixture"], { type: "audio/wav" }), "a.wav");
  return await fetch(`${gateway.baseUrl}/v1/audio/transcriptions`, {
    method: "POST",
    body,
  });
}

async function chatRequest(
  gateway: GatewayFixture,
  init: RequestInit = {},
  model = gateway.readConfig().activeLlmModel,
): Promise<Response> {
  return await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json", ...init.headers },
    body: JSON.stringify({
      model,
      messages: [{ role: "user", content: "fixture" }],
    }),
    ...init,
  });
}

test.each([
  {
    name: "STT",
    options: { sttEnabled: false },
    enable: (gateway: GatewayFixture) => {
      const config = gateway.readConfig();
      config.activeSttModel = STT_MODEL;
      config.selectedSttModels = [STT_MODEL];
      gateway.saveConfig(config);
    },
    disable: (gateway: GatewayFixture) => {
      const config = gateway.readConfig();
      config.activeSttModel = "";
      config.selectedSttModels = [];
      gateway.saveConfig(config);
    },
    request: transcriptionRequest,
    launches: (gateway: GatewayFixture) => gateway.readSttRuntimeLaunches(),
    waitForLaunch: (gateway: GatewayFixture, offset: number) =>
      gateway.waitForSttRuntimeLaunches(offset, 1),
    health: "stt" as const,
  },
  {
    name: "image",
    options: { imageEnabled: false },
    enable: (gateway: GatewayFixture) => {
      const config = gateway.readConfig();
      config.activeImageModel = IMAGE_MODEL;
      config.selectedImageModels = [IMAGE_MODEL];
      gateway.saveConfig(config);
    },
    disable: (gateway: GatewayFixture) => {
      const config = gateway.readConfig();
      config.activeImageModel = "";
      config.selectedImageModels = [];
      gateway.saveConfig(config);
    },
    request: imageRequest,
    launches: (gateway: GatewayFixture) => gateway.readImageRuntimeLaunches(),
    waitForLaunch: (gateway: GatewayFixture, offset: number) =>
      gateway.waitForImageRuntimeLaunches(offset, 1),
    health: "image" as const,
  },
])(
  "reconciles dynamic $name enablement and disablement without rebinding the gateway",
  async ({
    options,
    enable,
    disable,
    request,
    launches,
    waitForLaunch,
    health,
  }) => {
    const gateway = await startGatewayFixture(options);
    try {
      const initial = await request(gateway);
      expect(initial.status).toBe(501);
      await initial.text();

      const offset = (await launches(gateway)).length;
      enable(gateway);
      const enabled = await request(gateway);
      expect(enabled.status).toBe(200);
      await enabled.text();
      await waitForLaunch(gateway, offset);

      disable(gateway);
      const models = await fetch(`${gateway.baseUrl}/v1/models`);
      expect(models.status).toBe(200);
      await models.text();
      const disabled = await request(gateway);
      expect(disabled.status).toBe(501);
      await disabled.text();

      const snapshot = gatewayHealthSchema.parse(
        await (await fetch(`${gateway.baseUrl}/health`)).json(),
      );
      expect(snapshot.modalities[health]).toEqual({
        configured: false,
        state: "disabled",
      });
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 15_000 },
);

test(
  "replaces only the affected LLM supervisor and coalesces concurrent refreshes",
  async () => {
    const gateway = await startGatewayFixture();
    try {
      for (const request of [
        chatRequest(gateway),
        transcriptionRequest(gateway),
        imageRequest(gateway),
      ]) {
        const response = await request;
        expect(response.status).toBe(200);
        await response.text();
      }
      const [initialLlm, initialStt, initialImage] = await Promise.all([
        gateway.waitForLlmRuntimeLaunches(0, 1),
        gateway.waitForSttRuntimeLaunches(0, 1),
        gateway.waitForImageRuntimeLaunches(0, 1),
      ]);
      const llmOffset = initialLlm.length;
      const sttOffset = initialStt.length;
      const imageOffset = initialImage.length;

      const config = gateway.readConfig();
      config.parallel = config.parallel === 2 ? 3 : 2;
      gateway.saveConfig(config);
      const refreshes = await Promise.all(
        Array.from({ length: 4 }, async () => {
          const response = await fetch(`${gateway.baseUrl}/v1/models`);
          await response.text();
          return response.status;
        }),
      );
      expect(refreshes).toEqual([200, 200, 200, 200]);

      const response = await chatRequest(gateway);
      expect(response.status).toBe(200);
      await response.text();
      await gateway.waitForLlmRuntimeLaunches(llmOffset, 1);
      expect((await gateway.readSttRuntimeLaunches()).length).toBe(sttOffset);
      expect((await gateway.readImageRuntimeLaunches()).length).toBe(
        imageOffset,
      );
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 15_000 },
);

test(
  "drains a streamed response before replacing its configured LLM",
  async () => {
    const gateway = await startGatewayFixture();
    try {
      const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;
      const streamId = "live-config-drain";
      const response = await chatRequest(gateway, {
        headers: {
          "x-test-upstream": "controlled-stream",
          "x-test-stream-id": streamId,
        },
      });
      const reader = response.body!.getReader();
      expect((await reader.read()).done).toBe(false);
      await gateway.waitForLlmRuntimeLaunches(launchOffset, 1);

      const config = gateway.readConfig();
      config.parallel = config.parallel === 2 ? 3 : 2;
      gateway.saveConfig(config);
      let settled = false;
      const reconciliation = fetch(`${gateway.baseUrl}/v1/models`).then(
        async (models) => {
          settled = true;
          await models.text();
          return models;
        },
      );
      await Bun.sleep(75);
      expect(settled).toBe(false);

      gateway.closeControlledStream(streamId);
      while (!(await reader.read()).done) {}
      expect((await reconciliation).status).toBe(200);

      const next = await chatRequest(gateway);
      expect(next.status).toBe(200);
      await next.text();
      await gateway.waitForLlmRuntimeLaunches(launchOffset + 1, 1);
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 15_000 },
);

test(
  "reports a failed replacement, recovers on a later revision, and does not download newly selected artifacts during refresh",
  async () => {
    const replacementModel = "mistral-nemo-12b-instruct-q4_k_m";
    const replacementSpec = byId(replacementModel);
    if (!replacementSpec) throw new Error("Expected catalog model.");
    const replacementArtifact = primaryArtifact(replacementSpec);
    const gateway = await startGatewayFixture();
    try {
      const selectedPath = join(
        gateway.readConfig().llmModelsDir,
        replacementArtifact.filename,
      );
      expect(await Bun.file(selectedPath).exists()).toBe(false);
      const initialLlmLaunches = (await gateway.readLlmRuntimeLaunches())
        .length;
      const selected = gateway.readConfig();
      selected.selectedLlmModels = [
        ...selected.selectedLlmModels,
        replacementModel,
      ];
      gateway.saveConfig(selected);
      const models = await fetch(`${gateway.baseUrl}/v1/models`);
      expect(models.status).toBe(200);
      await models.text();
      expect(await Bun.file(selectedPath).exists()).toBe(false);
      expect((await gateway.readLlmRuntimeLaunches()).length).toBe(
        initialLlmLaunches,
      );

      await writeCompleteCatalogArtifact(
        gateway.readConfig().llmModelsDir,
        replacementModel,
      );
      await gateway.setLlmRuntimeFailure(true);
      gateway.setLlmBackendHealthy(false);
      const failedRequest = await chatRequest(gateway, {}, replacementModel);
      expect(failedRequest.status).toBe(503);
      await failedRequest.text();
      const failedHealth = gatewayHealthSchema.parse(
        await (await fetch(`${gateway.baseUrl}/health`)).json(),
      );
      expect(failedHealth.modalities.llm.state).toBe("failed");

      await gateway.setLlmRuntimeFailure(false);
      gateway.setLlmBackendHealthy(true);
      const recoveredRequest = await chatRequest(
        gateway,
        {},
        "qwen2.5-coder-1.5b-instruct-q4_k_m",
      );
      expect(recoveredRequest.status).toBe(200);
      await recoveredRequest.text();
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 15_000 },
);
