import { afterEach, expect, spyOn, test as bunTest } from "bun:test";
import {
  mkdtempSync,
  mkdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
  existsSync,
} from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { CATALOG, type ModelSpec } from "../../catalog";
import { DatabaseSession } from "../../db/client";
import { defaultConfig, saveConfig } from "../../manager";
import { RuntimeConfigController } from "../runtime/config-snapshot";
import { createRuntimeLifecycleSnapshot } from "../runtime/lifecycle-snapshot";
import type { RuntimeModality } from "../runtime/modality";
import { createModelManagement } from "./model-management";
import {
  modelManagementSchema,
  modelManagementRequestSchema,
} from "./model-management-contract";

// These fixtures append to the validated catalog and mock global fetch.
// Serial tests keep both shared resources scoped until installation settles.
const test = bunTest.serial;
const mutableCatalog = CATALOG as ModelSpec[];
const cleanup: (() => void)[] = [];
afterEach(() => {
  for (const dispose of cleanup.splice(0).reverse()) dispose();
});

test("management contract bundles for browsers and leaves catalog lookup to the core", async () => {
  const result = await Bun.build({
    entrypoints: [join(import.meta.dir, "model-management-contract.ts")],
    target: "browser",
  });
  expect(result.success).toBe(true);
  expect(
    modelManagementRequestSchema.safeParse({
      modelId: "not-in-catalog",
      action: "install",
    }).success,
  ).toBe(true);
  for (const modelId of ["", "a".repeat(201)])
    expect(
      modelManagementRequestSchema.safeParse({ modelId, action: "install" })
        .success,
    ).toBe(false);
});

function fixture() {
  const root = mkdtempSync(join(tmpdir(), "localbase-management-"));
  const database = new DatabaseSession();
  const config = defaultConfig(root, 16);
  saveConfig(database, config);
  const runtimeConfig = new RuntimeConfigController(database, root, config);
  const base = CATALOG[0];
  if (!base) throw new Error("Expected catalog fixture base");
  const model: ModelSpec = {
    ...base,
    modelId: `test-management-${crypto.randomUUID()}`,
    kind: "stt",
    inputModalities: ["audio"],
    outputModalities: ["text"],
    artifacts: [
      {
        filename: "fixture.bin",
        sourcePath: "fixture.bin",
        role: "primary",
        expectedSizeBytes: 3,
        sha256: "a".repeat(64),
      },
    ],
  };
  mutableCatalog.push(model);
  cleanup.push(() => {
    mutableCatalog.splice(mutableCatalog.indexOf(model), 1);
    database.close();
    rmSync(root, { recursive: true, force: true });
  });
  function snapshot(modality: RuntimeModality) {
    return createRuntimeLifecycleSnapshot({
      modality,
      configured: false,
      state: "disabled",
      modelId: null,
      runtimeId: null,
      admission: { kind: "unknown" },
      configuredSlots: null,
    });
  }
  const runtimes = {
    llm: snapshot("llm"),
    stt: snapshot("stt"),
    tts: snapshot("tts"),
    image: snapshot("image"),
    video: snapshot("video"),
  };
  const management = createModelManagement({
    runtimeConfig,
    lifecycle: () => runtimes,
  });
  const path = join(config.sttModelsDir, "fixture.bin");
  return {
    root,
    model,
    path,
    management,
    runtimeConfig,
    runtimes,
    config,
    database,
  };
}

test("full catalog contract includes installed, enabled, active and exact footprints", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  const response = modelManagementSchema.parse(await f.management.read());
  expect(response.models).toHaveLength(CATALOG.length);
  expect(
    response.models.find((entry) => entry.id === f.model.modelId),
  ).toMatchObject({
    installed: true,
    enabled: false,
    active: false,
    installedBytes: 3,
    downloadBytes: 3,
    operation: null,
  });
  expect(response.storage.availableBytes).toBeGreaterThan(0);
  expect(
    modelManagementRequestSchema.safeParse({
      modelId: f.model.modelId,
      action: "install",
      url: "https://example.com",
    }).success,
  ).toBe(false);
  await expect(f.management.run("../outside", "install")).rejects.toMatchObject(
    { code: "invalid_request" },
  );
});

test("enable and activate require installation; changes use fresh config and preserve unrelated settings", async () => {
  const f = fixture();
  await expect(
    f.management.run(f.model.modelId, "enable"),
  ).rejects.toMatchObject({ code: "conflict" });
  writeFileSync(f.path, "abc");
  await expect(
    f.management.run(f.model.modelId, "activate"),
  ).rejects.toMatchObject({ code: "conflict" });
  f.runtimeConfig.update((config) => {
    config.parallel = 2;
  });
  await f.management.run(f.model.modelId, "enable");
  await f.management.run(f.model.modelId, "enable");
  await f.management.run(f.model.modelId, "activate");
  expect(
    f.runtimeConfig
      .copy()
      .selectedSttModels.filter((id) => id === f.model.modelId),
  ).toHaveLength(1);
  expect(f.runtimeConfig.copy().activeSttModel).toBe(f.model.modelId);
  expect(f.runtimeConfig.copy().parallel).toBe(2);
  await f.management.run(f.model.modelId, "disable");
  expect(f.runtimeConfig.copy().activeSttModel).toBe("");
});

test("uninstall waits for disabled config and runtime reference release, then deletes exact files only", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  const unrelated = join(f.config.sttModelsDir, "keep.bin");
  writeFileSync(unrelated, "keep");
  await f.management.run(f.model.modelId, "enable");
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "conflict" });
  f.runtimes.stt = {
    ...f.runtimes.stt,
    modelId: f.model.modelId,
    state: "draining",
    admission: { kind: "known", accepting: false, activeCount: 1 },
  };
  await f.management.run(f.model.modelId, "disable");
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "conflict" });
  expect(existsSync(f.path)).toBe(true);
  f.runtimes.stt = { ...f.runtimes.stt, modelId: null };
  await f.management.run(f.model.modelId, "uninstall");
  expect(existsSync(f.path)).toBe(false);
  expect(existsSync(unrelated)).toBe(true);
  expect(existsSync(f.config.sttModelsDir)).toBe(true);
});

test("uninstall preserves artifacts referenced by any other catalog model", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  writeFileSync(`${f.path}.partial`, "ab");
  const other = { ...f.model, modelId: `${f.model.modelId}-shared` };
  mutableCatalog.push(other);
  cleanup.push(() => {
    mutableCatalog.splice(mutableCatalog.indexOf(other), 1);
  });
  await f.management.run(f.model.modelId, "uninstall");
  expect(existsSync(f.path)).toBe(true);
  expect(existsSync(`${f.path}.partial`)).toBe(true);
});

test("partial downloads count toward disk footprint and can be uninstalled", async () => {
  const f = fixture();
  writeFileSync(`${f.path}.partial`, "ab");
  const entry = async () =>
    (await f.management.read()).models.find(
      (item) => item.id === f.model.modelId,
    );
  expect(await entry()).toMatchObject({ installed: false, installedBytes: 2 });
  writeFileSync(f.path, "abc");
  expect(await entry()).toMatchObject({ installed: true, installedBytes: 5 });
  rmSync(f.path);
  await f.management.run(f.model.modelId, "uninstall");
  expect(existsSync(`${f.path}.partial`)).toBe(false);
  expect(await entry()).toMatchObject({ installed: false, installedBytes: 0 });
});

test("unsafe partial target prevents deletion of a valid final artifact", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  mkdirSync(`${f.path}.partial`);
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "unsafe_path" });
  expect(existsSync(f.path)).toBe(true);
  expect(existsSync(`${f.path}.partial`)).toBe(true);
});

test.each(["symlink", "directory"])(
  "rejects %s artifact targets without removing them",
  async (kind) => {
    const f = fixture();
    if (kind === "directory") mkdirSync(f.path);
    else symlinkSync(join(f.root, "absent-outside-target"), f.path);
    await expect(
      f.management.run(f.model.modelId, "uninstall"),
    ).rejects.toMatchObject({ code: "unsafe_path" });
    await expect(
      f.management.run(f.model.modelId, "install"),
    ).rejects.toMatchObject({ code: "unsafe_path" });
  },
);

test("rejects symlink directory escapes", async () => {
  const f = fixture();
  rmSync(f.config.sttModelsDir, { recursive: true });
  symlinkSync(f.config.llmModelsDir, f.config.sttModelsDir);
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "unsafe_path" });
});

test.each(["fixture.bin.partial", ".checksums.json"])(
  "rejects installer sidecar symlink %s",
  async (filename) => {
    const f = fixture();
    symlinkSync(join(f.root, "outside"), join(f.config.sttModelsDir, filename));
    await expect(
      f.management.run(f.model.modelId, "install"),
    ).rejects.toMatchObject({ code: "unsafe_path" });
  },
);

test("validates every artifact before uninstalling any file", async () => {
  const f = fixture();
  const artifact = f.model.artifacts[0];
  if (!artifact) throw new Error("Expected fixture artifact");
  f.model.artifacts.push({
    ...artifact,
    filename: "second.bin",
    sourcePath: "second.bin",
    role: "supplementary",
  });
  writeFileSync(f.path, "abc");
  mkdirSync(join(f.config.sttModelsDir, "second.bin"));
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "unsafe_path" });
  expect(existsSync(f.path)).toBe(true);
});

test("requires switching away from the default LLM before disabling it", async () => {
  const f = fixture();
  await expect(
    f.management.run(f.config.activeLlmModel, "disable"),
  ).rejects.toMatchObject({ code: "conflict" });
  expect(f.runtimeConfig.copy().activeLlmModel).toBe(f.config.activeLlmModel);
});

test("disable preserves external persisted updates and read refreshes enabled state", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  await f.management.run(f.model.modelId, "enable");
  const external = f.runtimeConfig.copy();
  external.parallel = 3;
  saveConfig(f.database, external);
  expect(f.runtimeConfig.copy().parallel).not.toBe(3);
  await f.management.run(f.model.modelId, "disable");
  expect(f.runtimeConfig.copy().parallel).toBe(3);
  external.selectedSttModels.push(f.model.modelId);
  external.selectedSttModels = [...new Set(external.selectedSttModels)];
  saveConfig(f.database, external);
  expect(
    (await f.management.read()).models.find(
      (entry) => entry.id === f.model.modelId,
    )?.enabled,
  ).toBe(true);
});

test("uninstall rejects externally enabled models even when the controller was stale", async () => {
  const f = fixture();
  writeFileSync(f.path, "abc");
  const external = f.runtimeConfig.copy();
  external.selectedSttModels.push(f.model.modelId);
  saveConfig(f.database, external);
  expect(f.runtimeConfig.copy().selectedSttModels).not.toContain(
    f.model.modelId,
  );
  await expect(
    f.management.run(f.model.modelId, "uninstall"),
  ).rejects.toMatchObject({ code: "conflict" });
  expect(existsSync(f.path)).toBe(true);
});

test("install rejects enabled nonresident targets before the installer runs", async () => {
  const f = fixture();
  const external = f.runtimeConfig.copy();
  external.selectedSttModels.push(f.model.modelId);
  saveConfig(f.database, external);
  const reason =
    "Disable this model and wait for the runtime to release it before installing.";
  await expect(
    f.management.run(f.model.modelId, "install"),
  ).rejects.toMatchObject({ code: "conflict", message: reason });
  expect(
    (await f.management.read()).models.find(
      (entry) => entry.id === f.model.modelId,
    ),
  ).toMatchObject({
    canInstall: false,
    installUnavailableReason: reason,
    operation: null,
  });
  expect(existsSync(f.path)).toBe(false);
});

test.each(["enabled", "referenced"])(
  "install rejects destructive artifact collisions with an %s catalog model",
  async (state) => {
    const f = fixture();
    const other = {
      ...f.model,
      modelId: `${f.model.modelId}-collision`,
      artifacts: f.model.artifacts.map((artifact) => ({
        ...artifact,
        sha256: "b".repeat(64),
      })),
    } satisfies ModelSpec;
    mutableCatalog.push(other);
    cleanup.push(() => {
      mutableCatalog.splice(mutableCatalog.indexOf(other), 1);
    });
    if (state === "enabled") {
      const external = f.runtimeConfig.copy();
      external.selectedSttModels.push(other.modelId);
      saveConfig(f.database, external);
    } else {
      f.runtimes.stt = { ...f.runtimes.stt, modelId: other.modelId };
    }
    const reason = `Disable ${other.modelId} and wait for its runtime to release shared artifacts before installing.`;
    await expect(
      f.management.run(f.model.modelId, "install"),
    ).rejects.toMatchObject({ code: "conflict", message: reason });
    expect(
      (await f.management.read()).models.find(
        (entry) => entry.id === f.model.modelId,
      ),
    ).toMatchObject({ canInstall: false, installUnavailableReason: reason });
    expect(existsSync(f.path)).toBe(false);
  },
);

test("rejects missing authority and insufficient disk before downloads", async () => {
  const f = fixture();
  const artifact = f.model.artifacts[0];
  if (!artifact) throw new Error("Expected fixture artifact");
  artifact.sha256 = undefined;
  expect(
    (await f.management.read()).models.find(
      (entry) => entry.id === f.model.modelId,
    ),
  ).toMatchObject({
    canInstall: false,
    installUnavailableReason:
      "Catalog model lacks authoritative sizes or SHA-256 hashes.",
  });
  await expect(
    f.management.run(f.model.modelId, "install"),
  ).rejects.toMatchObject({ code: "invalid_request" });
  artifact.sha256 = "a".repeat(64);
  artifact.expectedSizeBytes = Number.MAX_SAFE_INTEGER;
  expect(
    (await f.management.read()).models.find(
      (entry) => entry.id === f.model.modelId,
    ),
  ).toMatchObject({
    canInstall: false,
    installUnavailableReason: "Insufficient storage for model installation.",
  });
  await expect(
    f.management.run(f.model.modelId, "install"),
  ).rejects.toMatchObject({ code: "insufficient_storage" });
});

test("background install admits one operation and contains failures without exposing secrets", async () => {
  const f = fixture();
  let rejectFetch: (reason: Error) => void = () => {
    throw new Error("Fetch not started");
  };
  const started = Promise.withResolvers<void>();
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(
    Object.assign(
      () => {
        started.resolve();
        return new Promise<Response>((_resolve, reject) => {
          rejectFetch = reject;
        });
      },
      { preconnect: fetch.preconnect },
    ),
  );
  cleanup.push(() => fetchSpy.mockRestore());
  const accepted = await f.management.run(f.model.modelId, "install");
  expect(accepted.state).toBe("running");
  expect(
    (await f.management.read()).models.find(
      (entry) => entry.id === f.model.modelId,
    ),
  ).toMatchObject({
    canInstall: false,
    installUnavailableReason: "Wait for the current installation to finish.",
  });
  await expect(
    f.management.run(f.model.modelId, "install"),
  ).rejects.toMatchObject({ code: "conflict" });
  await started.promise;
  rejectFetch(new Error("secret-token"));
  await f.management.whenIdle();
  const operation = (await f.management.read()).models.find(
    (entry) => entry.id === f.model.modelId,
  )?.operation;
  expect(operation?.state).toBe("failed");
  expect(operation?.detail).not.toContain("secret-token");
  expect(accepted.state).toBe("running");
});

test("availability reasons distinguish runtime references and unsafe storage", async () => {
  const f = fixture();
  const entry = async () =>
    (await f.management.read()).models.find(
      (item) => item.id === f.model.modelId,
    );
  expect(await entry()).toMatchObject({
    canInstall: true,
    installUnavailableReason: null,
  });
  f.runtimes.stt = { ...f.runtimes.stt, modelId: f.model.modelId };
  expect(await entry()).toMatchObject({
    canInstall: false,
    installUnavailableReason:
      "Disable this model and wait for the runtime to release it before installing.",
  });
  f.runtimes.stt = { ...f.runtimes.stt, modelId: null };
  symlinkSync(join(f.root, "private-target"), `${f.path}.partial`);
  const unavailable = await entry();
  expect(unavailable?.canInstall).toBe(false);
  expect(unavailable?.installUnavailableReason).toContain("symlink");
  expect(unavailable?.installUnavailableReason).not.toContain(f.root);
});

test("allows installation while another enabled model uses an identical shared artifact", async () => {
  const f = fixture();
  const shared = {
    ...f.model,
    modelId: `${f.model.modelId}-shared`,
    artifacts: f.model.artifacts.map((artifact) => ({ ...artifact })),
  } satisfies ModelSpec;
  mutableCatalog.push(shared);
  cleanup.push(() => {
    mutableCatalog.splice(mutableCatalog.indexOf(shared), 1);
  });
  f.runtimeConfig.update((config) => {
    config.selectedSttModels.push(shared.modelId);
    config.activeSttModel = shared.modelId;
  });

  const entry = async () =>
    (await f.management.read()).models.find(
      (item) => item.id === f.model.modelId,
    );
  expect(await entry()).toMatchObject({
    canInstall: true,
    installUnavailableReason: null,
  });

  const artifact = shared.artifacts[0];
  if (!artifact) throw new Error("Expected shared artifact fixture");
  artifact.sha256 = "b".repeat(64);
  expect(await entry()).toMatchObject({
    canInstall: false,
    installUnavailableReason: `Disable ${shared.modelId} and wait for its runtime to release shared artifacts before installing.`,
  });
});

test("background installation verifies fixture bytes and publishes completion", async () => {
  const f = fixture();
  const artifact = f.model.artifacts[0];
  if (!artifact) throw new Error("Expected fixture artifact");
  artifact.sha256 = new Bun.CryptoHasher("sha256").update("abc").digest("hex");
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(
    Object.assign(
      async () => new Response("abc", { headers: { "Content-Length": "3" } }),
      { preconnect: fetch.preconnect },
    ),
  );
  cleanup.push(() => fetchSpy.mockRestore());
  expect((await f.management.run(f.model.modelId, "install")).state).toBe(
    "running",
  );
  await f.management.whenIdle();
  const entry = (await f.management.read()).models.find(
    (item) => item.id === f.model.modelId,
  );
  expect(entry).toMatchObject({
    installed: true,
    installedBytes: 3,
    operation: { state: "complete", downloadedBytes: 3, totalBytes: 3 },
  });
  expect(await Bun.file(f.path).text()).toBe("abc");
  await expect(
    f.management.run(f.model.modelId, "enable"),
  ).resolves.toMatchObject({ state: "complete" });
});
