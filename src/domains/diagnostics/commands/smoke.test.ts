import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { AppContext } from "../../../context";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig, loadApiKeys, saveConfig } from "../../../manager";
import type { CommandExecution } from "../../app/commands/framework";
import { smokeResultSchema } from "../../app/commands/results";
import { LOCALBASE_VERSION } from "../../../version";
import {
  registerGatewayFixtureCleanup,
  startGatewayFixture,
  writeCompleteCatalogArtifact,
  type GatewayFixture,
} from "../../../test/gateway-fixture";
import { runSmoke } from "./smoke";

registerGatewayFixtureCleanup();

const REPLACEMENT_MODEL = "mistral-nemo-12b-instruct-q4_k_m";
const RESIDENT_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";

const execution: CommandExecution = {
  global: { nonInteractive: true, json: true },
  output: { info() {}, error() {}, lifecycle() {} },
};

async function runSmokeCli(gateway: GatewayFixture, args: string[] = []) {
  const child = Bun.spawn(
    [gateway.cliPath, "smoke", "--root", gateway.root, "--json", ...args],
    { stdout: "pipe", stderr: "pipe" },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  const envelope = JSON.parse(stdout.trim()) as { ok: boolean; data: unknown };
  return {
    exitCode,
    stdout,
    stderr,
    ok: envelope.ok,
    result: smokeResultSchema.parse(envelope.data),
  };
}

function storedKeyNames(gateway: GatewayFixture): string[] {
  const database = new DatabaseSession();
  try {
    return loadApiKeys(database, gateway.readConfig()).map((key) => key.name);
  } finally {
    database.close();
  }
}

test(
  "smoke authenticates with a throwaway key, completes a chat, and removes the key",
  async () => {
    const gateway = await startGatewayFixture({ auth: { mode: "bearer" } });
    try {
      const run = await runSmokeCli(gateway);
      expect(run.exitCode).toBe(0);
      expect(run.ok).toBe(true);
      expect(run.result).toMatchObject({
        passed: true,
        gatewayUrl: gateway.baseUrl,
        failure: null,
      });
      expect(run.result.steps.map((step) => [step.name, step.ok])).toEqual([
        ["health", true],
        ["chat", true],
      ]);
      expect(run.result.steps[1]?.model).toBe(RESIDENT_MODEL);
      const chat = gateway.upstreamRequests.find(
        (request) => request.path === "/v1/chat/completions",
      );
      expect(JSON.parse(chat?.body ?? "{}")).toMatchObject({ max_tokens: 4 });
      expect(`${run.stdout}${run.stderr}`).not.toMatch(/lb_[A-Za-z0-9]/);
      expect(storedKeyNames(gateway)).toEqual(["conformance"]);
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 60_000 },
);

test(
  "smoke switches to a named model and back only when asked",
  async () => {
    const gateway = await startGatewayFixture();
    try {
      await writeCompleteCatalogArtifact(
        gateway.readConfig().llmModelsDir,
        REPLACEMENT_MODEL,
      );
      const config = gateway.readConfig();
      config.selectedLlmModels = [
        ...config.selectedLlmModels,
        REPLACEMENT_MODEL,
      ];
      gateway.saveConfig(config);

      const run = await runSmokeCli(gateway, [
        "--switch-to",
        REPLACEMENT_MODEL,
      ]);
      expect(run.exitCode).toBe(0);
      expect(
        run.result.steps.map((step) => [step.name, step.model, step.ok]),
      ).toEqual([
        ["health", null, true],
        ["chat", RESIDENT_MODEL, true],
        ["switch", REPLACEMENT_MODEL, true],
        ["switch-back", RESIDENT_MODEL, true],
      ]);
      expect(gateway.readConfig().activeLlmModel).toBe(RESIDENT_MODEL);
      expect(storedKeyNames(gateway)).toEqual([]);
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 60_000 },
);

test(
  "smoke exits non-zero with the reason when inference fails",
  async () => {
    const gateway = await startGatewayFixture();
    try {
      await gateway.setLlmRuntimeFailure(true);
      gateway.setLlmBackendHealthy(false);
      const run = await runSmokeCli(gateway, ["--timeout", "20"]);
      expect(run.exitCode).toBe(1);
      expect(run.ok).toBe(true);
      expect(run.result.passed).toBe(false);
      expect(run.result.failure).toStartWith(`chat ${RESIDENT_MODEL}: HTTP 5`);
      expect(run.stderr).toContain("Smoke check failed");
      expect(storedKeyNames(gateway)).toEqual([]);
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 60_000 },
);

test("smoke reports a missing gateway without a stack trace", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-smoke-"));
  const database = new DatabaseSession();
  try {
    const config = defaultConfig(join(directory, "root"));
    saveConfig(database, config);
    const { data, exitCode } = await runSmoke(
      { timeout: 5 },
      { config, database } as unknown as AppContext,
      execution,
    );
    expect(exitCode).toBe(1);
    expect(data.passed).toBe(false);
    expect(data.failure).toStartWith("health: no gateway is running for ");
    expect(loadApiKeys(database, config)).toEqual([]);
  } finally {
    database.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("smoke flags a switch the gateway rolled back", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-smoke-"));
  const database = new DatabaseSession();
  const authorizations = new Set<string>();
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const path = new URL(request.url).pathname;
      if (path === "/health")
        return Response.json({
          status: "ok",
          version: LOCALBASE_VERSION,
          modalities: { llm: { configured: true, state: "running" } },
        });
      authorizations.add(request.headers.get("authorization") ?? "");
      if (path === "/_localbase/models")
        return Response.json({
          object: "list",
          data: [RESIDENT_MODEL, REPLACEMENT_MODEL].map((id) => ({
            id,
            catalog: { kind: "llm" },
            device: {
              runtime: id === RESIDENT_MODEL ? { state: "running" } : null,
            },
          })),
        });
      const body = (await request.json()) as { model: string };
      if (body.model === REPLACEMENT_MODEL)
        return Response.json(
          {
            error: {
              message: "Not enough memory to load the model.",
              code: "insufficient_memory",
            },
          },
          { status: 503 },
        );
      return Response.json({
        choices: [{ message: { role: "assistant", content: "OK" } }],
        usage: { completion_tokens: 1 },
      });
    },
  });
  try {
    const config = defaultConfig(join(directory, "root"));
    saveConfig(database, config);
    const { data, exitCode } = await runSmoke(
      { timeout: 5, switchTo: REPLACEMENT_MODEL },
      { config, database } as unknown as AppContext,
      execution,
      { resolveBaseUrl: async () => `http://127.0.0.1:${server.port}` },
    );
    expect(exitCode).toBe(1);
    expect(data.steps.map((step) => [step.name, step.ok])).toEqual([
      ["health", true],
      ["chat", true],
      ["switch", false],
    ]);
    expect(data.failure).toBe(
      `switch ${REPLACEMENT_MODEL}: HTTP 503: Not enough memory to load the model. (insufficient_memory); ${RESIDENT_MODEL} is still resident`,
    );
    expect([...authorizations]).toHaveLength(1);
    expect([...authorizations][0]).toMatch(/^Bearer lb_/);
    expect(loadApiKeys(database, config)).toEqual([]);
  } finally {
    server.stop(true);
    database.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
