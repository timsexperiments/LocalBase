import { expect, test } from "bun:test";

import {
  startGatewayFixture,
  writeCompleteCatalogArtifact,
} from "../../test/gateway-fixture";

const request = (baseUrl: string, model: string, content: string) =>
  fetch(`${baseUrl}/v1/chat/completions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      model,
      stream: true,
      messages: [{ role: "user", content }],
    }),
  });

test("compiled gateway releases a crashed backend stream and recovers", async () => {
  const gateway = await startGatewayFixture({ llmRuntimeHttpBackend: true });
  try {
    const firstModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    const launchOffset = (await gateway.readLlmRuntimeLaunches()).length;
    const response = await request(
      gateway.baseUrl,
      firstModel,
      "wait for crash",
    );
    expect(response.status).toBe(200);
    await gateway.waitForLlmRuntimeStart();
    const reader = response.body?.getReader();
    expect(reader).toBeDefined();
    if (!reader) throw new Error("Gateway did not return a stream body.");
    await gateway.waitForLlmFirstEvent();
    expect((await reader.read()).done).toBe(false);
    await gateway.crashLlmRuntime();
    await expect(reader.read()).resolves.toMatchObject({ done: true });

    const recovered = await request(gateway.baseUrl, firstModel, "recover");
    expect(recovered.status).toBe(200);
    await gateway.waitForLlmRuntimeLaunches(launchOffset, 2);
    const recoveredReader = recovered.body?.getReader();
    expect(recoveredReader).toBeDefined();
    if (!recoveredReader)
      throw new Error("Gateway did not recover an SSE body.");
    expect((await recoveredReader.read()).done).toBe(false);
    await gateway.crashLlmRuntime();
    await expect(recoveredReader.read()).resolves.toMatchObject({ done: true });

    const replacementModel = "qwen2.5-coder-7b-instruct-q4_k_m";
    const config = gateway.readConfig();
    gateway.saveConfig({
      ...config,
      selectedLlmModels: [firstModel, replacementModel],
    });
    await writeCompleteCatalogArtifact(config.llmModelsDir, replacementModel);

    const replacement = await request(
      gateway.baseUrl,
      replacementModel,
      "replace after crash",
    );
    expect(replacement.status).toBe(200);
    await gateway.waitForLlmRuntimeLaunches(launchOffset, 3);
    const replacementReader = replacement.body?.getReader();
    expect(replacementReader).toBeDefined();
    if (!replacementReader)
      throw new Error("Gateway did not replace the SSE body.");
    expect((await replacementReader.read()).done).toBe(false);
    await replacementReader.cancel();
  } finally {
    await gateway.stop();
  }
}, 10_000);
