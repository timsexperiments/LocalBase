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
import { applyCatalogLayers } from "../../../catalog/sources";
import { loadCatalogSources } from "../../../catalog/user-catalog";
import { runSmoke } from "./smoke";

registerGatewayFixtureCleanup();

const REPLACEMENT_MODEL = "mistral-nemo-12b-instruct-q4_k_m";
const RESIDENT_MODEL = "qwen2.5-coder-1.5b-instruct-q4_k_m";
const DECISION_MODEL = "decision-fixture-q4_k_m";
const decisionCatalogEntry = {
  modelId: DECISION_MODEL,
  displayName: "Decision Fixture",
  kind: "llm",
  provider: "Fixture",
  family: "Decision",
  version: "9B",
  size: "9B",
  quant: "Q4_K_M",
  storageGb: 0.01,
  source: "https://huggingface.co/fixture/decision-GGUF",
  repositoryRevision: "0123456789abcdef0123456789abcdef01234567",
  artifacts: [
    {
      sourcePath: "decision-fixture.gguf",
      filename: "decision-fixture.gguf",
      expectedSizeBytes: 1024,
      sha256:
        "0000000000000000000000000000000000000000000000000000000000000000",
      role: "primary",
    },
  ],
  llmRuntime: { capability: "decision-only" },
  inputModalities: ["text"],
  outputModalities: ["text"],
  contextWindowTokens: 8192,
  capabilities: ["decision"],
  commercialStatus: "open",
  catch: "Decision endpoint only.",
  notes: "Test decision model.",
};

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
    // Only the root matters, so no catalog lookup can reject a fixture model.
    return loadApiKeys(database, defaultConfig(gateway.root)).map(
      (key) => key.name,
    );
  } finally {
    database.close();
  }
}

/** Reads or edits fixture config with the fixture's user catalog applied. */
function withFixtureCatalog<T>(gateway: GatewayFixture, work: () => T): T {
  loadCatalogSources(gateway.root);
  try {
    return work();
  } finally {
    applyCatalogLayers([]);
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
  "smoke asks a decision model one typed question instead of chatting",
  async () => {
    const gateway = await startGatewayFixture({
      auth: { mode: "bearer" },
      llmCatalogEntry: decisionCatalogEntry,
    });
    try {
      const run = await runSmokeCli(gateway);
      expect(run.exitCode).toBe(0);
      expect(
        run.result.steps.map((step) => [step.name, step.model, step.ok]),
      ).toEqual([
        ["health", null, true],
        ["decision", DECISION_MODEL, true],
      ]);
      expect(run.result.steps[1]?.detail).toBe(
        "answered a yes/no question (noul 0.75)",
      );
      const paths = gateway.upstreamRequests.map((request) => request.path);
      expect(paths).not.toContain("/v1/chat/completions");
      const decision = gateway.upstreamRequests.find(
        (request) => request.path === "/v1/systemone",
      );
      expect(JSON.parse(decision?.body ?? "{}")).toMatchObject({
        model: DECISION_MODEL,
        questions: { ok: { type: "noul" } },
      });
      expect(`${run.stdout}${run.stderr}`).not.toMatch(/lb_[A-Za-z0-9]/);
      expect(storedKeyNames(gateway)).toEqual(["conformance"]);
    } finally {
      await gateway.stop();
    }
  },
  { timeout: 60_000 },
);

test(
  "smoke switches to a decision model with a typed question and back",
  async () => {
    const gateway = await startGatewayFixture({
      llmCatalogEntry: decisionCatalogEntry,
    });
    try {
      await writeCompleteCatalogArtifact(
        defaultConfig(gateway.root).llmModelsDir,
        RESIDENT_MODEL,
      );
      withFixtureCatalog(gateway, () => {
        const config = gateway.readConfig();
        config.selectedLlmModels = [
          ...config.selectedLlmModels,
          RESIDENT_MODEL,
        ];
        gateway.saveConfig(config);
      });
      // Make the chat model resident so the decision model is the switch target.
      const warm = await fetch(`${gateway.baseUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          model: RESIDENT_MODEL,
          messages: [{ role: "user", content: "hi" }],
        }),
      });
      expect(warm.status).toBe(200);
      await warm.text();

      const run = await runSmokeCli(gateway, ["--switch-to", DECISION_MODEL]);
      expect(run.exitCode).toBe(0);
      expect(
        run.result.steps.map((step) => [step.name, step.model, step.ok]),
      ).toEqual([
        ["health", null, true],
        ["chat", RESIDENT_MODEL, true],
        ["switch", DECISION_MODEL, true],
        ["switch-back", RESIDENT_MODEL, true],
      ]);
      const paths = gateway.upstreamRequests.map((request) => request.path);
      expect(paths.filter((path) => path === "/v1/systemone")).toHaveLength(1);
      expect(
        withFixtureCatalog(gateway, () => gateway.readConfig().activeLlmModel),
      ).toBe(RESIDENT_MODEL);
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
