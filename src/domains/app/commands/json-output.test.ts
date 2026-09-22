import { acknowledgeStaticConfiguration } from "../../config/activation";
import { DatabaseSession } from "../../../db/client";
import { defaultConfig } from "../../../manager";
import {
  configurationDocument,
  parseConfiguration,
  renderConfiguration,
} from "../../config/declarative";
import { expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import {
  defaultApiKeyScopes,
  permissionSchema,
} from "../../auth/authorization";
import {
  accessConfigureResultSchema,
  accessGithubListResultSchema,
  accessGithubRemoveResultSchema,
  accessOidcListResultSchema,
  accessOidcRemoveResultSchema,
  accessPolicyApplyResultSchema,
  accessPolicyClearResultSchema,
  accessPolicyTestResultSchema,
  accessShowResultSchema,
  keyMetadataResultSchema,
  keySecretResultSchema,
  keysListResultSchema,
} from "./results";
import { defaultBrowserPermissions } from "../../auth/browser-access";

const projectRoot = join(import.meta.dirname, "../../../..");

type CliResult = { exitCode: number; stdout: string; stderr: string };

async function compileCli(outputPath: string): Promise<void> {
  const build = Bun.spawn(
    [
      process.execPath,
      "build",
      "src/cli.ts",
      "--compile",
      "--no-compile-autoload-dotenv",
      "--no-compile-autoload-bunfig",
      "--asset-naming=[dir]/[name].[ext]",
      `--outfile=${outputPath}`,
    ],
    {
      cwd: projectRoot,
      stdout: "pipe",
      stderr: "pipe",
    },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    build.exited,
    new Response(build.stdout).text(),
    new Response(build.stderr).text(),
  ]);
  if (exitCode !== 0) {
    throw new Error(`Could not compile CLI:\n${stdout}${stderr}`);
  }
}

async function runCli(
  executable: string,
  args: string[],
  stdin?: string,
  environment: Record<string, string> = {},
): Promise<CliResult> {
  const runtimeDirectory = join(dirname(executable), "runtime");
  await mkdir(runtimeDirectory, { recursive: true, mode: 0o700 });
  const child = Bun.spawn([executable, ...args], {
    cwd: projectRoot,
    stdin: stdin === undefined ? "ignore" : new Blob([stdin]),
    stdout: "pipe",
    stderr: "pipe",
    env: {
      ...process.env,
      ...environment,
      XDG_RUNTIME_DIR: runtimeDirectory,
    },
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { exitCode, stdout, stderr };
}

function jsonDocument(output: string): Record<string, unknown> {
  const lines = output.trim().split("\n").filter(Boolean);
  expect(lines).toHaveLength(1);
  return JSON.parse(lines[0]) as Record<string, unknown>;
}

test(
  "compiled CLI emits pure JSON envelopes and redacts persistent secrets",
  async () => {
    const directory = mkdtempSync(join(tmpdir(), "local-base-json-cli-"));
    const executable = join(directory, "local-base");
    const root = join(directory, "data");

    try {
      await compileCli(executable);

      const configured = await runCli(executable, [
        "--root",
        root,
        "configure",
        "--json",
        "--defaults",
        "--no-create-key",
        "--hf-token",
        "private-token",
        "--llm-models",
        "qwen2.5-coder-7b-instruct-q4_k_m,mistral-nemo-12b-instruct-q4_k_m",
        "--active-llm",
        "mistral-nemo-12b-instruct-q4_k_m",
        "--parallel",
        "3",
        "--ctx-size",
        "8192",
      ]);
      expect(configured.exitCode).toBe(0);
      const configuration = jsonDocument(configured.stdout);
      expect(configuration.ok).toBe(true);
      expect(configured.stdout).not.toContain("private-token");
      expect(configured.stderr).toContain("Saved configuration");

      const created = await runCli(executable, [
        "--root",
        root,
        "--json",
        "keys",
        "create",
        "--name",
        "automation",
      ]);
      const createdData = jsonDocument(created.stdout).data as {
        secret: string;
        key: { id: string };
      };
      expect(createdData.secret).toMatch(/^lb_/);
      expect(
        keySecretResultSchema.parse(jsonDocument(created.stdout).data).key
          .scopes,
      ).toEqual(defaultApiKeyScopes);
      expect(created.stderr).not.toContain(createdData.secret);

      const rotated = await runCli(executable, [
        "--root",
        root,
        "--json",
        "keys",
        "rotate",
        createdData.key.id,
      ]);
      const rotatedData = jsonDocument(rotated.stdout).data as {
        secret: string;
      };
      expect(rotatedData.secret).toMatch(/^lb_/);
      expect(rotated.stderr).not.toContain(rotatedData.secret);

      const listed = await runCli(executable, [
        "--root",
        root,
        "keys",
        "list",
        "--json",
      ]);
      expect(listed.exitCode).toBe(0);
      expect(listed.stdout).not.toContain(createdData.secret);
      expect(listed.stdout).not.toContain("keyHash");

      const browserAccess = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "cloudflare",
        "--team-domain",
        "team.cloudflareaccess.com",
        "--audience",
        "audience",
        "--origin",
        "https://localbase.example.com",
      ]);
      expect(browserAccess.exitCode).toBe(0);
      expect(
        accessConfigureResultSchema.parse(
          jsonDocument(browserAccess.stdout).data,
        ).config.permissions,
      ).toEqual(defaultBrowserPermissions);
      const restrictedBrowserAccess = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "cloudflare",
        "--team-domain",
        "updated.cloudflareaccess.com",
        "--audience",
        "updated-audience",
        "--origin",
        "https://localbase.example.com",
        "--permissions",
        "inference:chat",
      ]);
      expect(restrictedBrowserAccess.exitCode).toBe(0);
      const updatedBrowserAccess = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "cloudflare",
        "--team-domain",
        "team.cloudflareaccess.com",
        "--audience",
        "audience",
        "--origin",
        "https://localbase.example.com",
      ]);
      expect(
        accessConfigureResultSchema.parse(
          jsonDocument(updatedBrowserAccess.stdout).data,
        ).config.permissions,
      ).toEqual(["inference:chat"]);
      const shownAccess = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "show",
      ]);
      expect(
        accessShowResultSchema.parse(jsonDocument(shownAccess.stdout).data)
          .config,
      ).toMatchObject({ origin: "https://localbase.example.com" });
      const oidcSecret = "oidc-secret-value";
      const oidcAccess = await runCli(
        executable,
        [
          "--root",
          root,
          "--json",
          "access",
          "oidc",
          "add",
          "--id",
          "primary",
          "--name",
          "Primary",
          "--issuer",
          "https://identity.example.com/tenant",
          "--client-id",
          "localbase-client",
          "--client-secret-env",
          "TEST_OIDC_SECRET",
          "--origin",
          "https://localbase.example.com",
          "--permissions",
          "inference:chat",
        ],
        undefined,
        { TEST_OIDC_SECRET: oidcSecret },
      );
      expect(oidcAccess.exitCode).toBe(0);
      const oidcOutput = accessConfigureResultSchema.parse(
        jsonDocument(oidcAccess.stdout).data,
      );
      expect(oidcOutput.config.provider).toEqual({
        kind: "direct",
        registrations: [
          {
            kind: "oidc",
            id: "primary",
            name: "Primary",
            issuer: "https://identity.example.com/tenant",
            clientId: "localbase-client",
            clientAuthentication: "client-secret-basic",
          },
        ],
      });
      expect(oidcOutput.config.permissions).toEqual(["inference:chat"]);
      expect(oidcAccess.stdout).not.toContain(oidcSecret);
      expect(oidcAccess.stderr).not.toContain(oidcSecret);
      const shownOidc = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "show",
      ]);
      expect(shownOidc.stdout).not.toContain(oidcSecret);
      expect(
        accessShowResultSchema.parse(jsonDocument(shownOidc.stdout).data).config
          ?.provider,
      ).toEqual(oidcOutput.config.provider);
      const githubSecret = "github-secret-value";
      const githubAccess = await runCli(
        executable,
        [
          "--root",
          root,
          "--json",
          "access",
          "github",
          "add",
          "--id",
          "github",
          "--name",
          "GitHub",
          "--client-id",
          "github-client",
          "--client-secret-env",
          "TEST_GITHUB_SECRET",
          "--origin",
          "https://localbase.example.com",
        ],
        undefined,
        { TEST_GITHUB_SECRET: githubSecret },
      );
      expect(githubAccess.exitCode).toBe(0);
      expect(githubAccess.stdout).not.toContain(githubSecret);
      const listedGithub = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "github",
        "list",
      ]);
      expect(
        accessGithubListResultSchema.parse(
          jsonDocument(listedGithub.stdout).data,
        ).registrations,
      ).toEqual([
        {
          kind: "github-oauth",
          id: "github",
          name: "GitHub",
          clientId: "github-client",
          clientAuthentication: "client-secret",
        },
      ]);
      const removedGithub = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "github",
        "remove",
        "github",
      ]);
      expect(
        accessGithubRemoveResultSchema.parse(
          jsonDocument(removedGithub.stdout).data,
        ).config?.provider,
      ).toEqual(oidcOutput.config.provider);
      const policyPath = join(directory, "browser-policy.json");
      await writeFile(
        policyPath,
        JSON.stringify({
          roles: [
            {
              name: "admin",
              permissions: ["access:manage", "models:manage"],
            },
            { name: "user", permissions: ["inference:chat"] },
          ],
          bindings: [
            {
              kind: "subject",
              role: "admin",
              issuer: "https://identity.example.com/tenant",
              subject: "owner",
            },
            {
              kind: "email-domain",
              role: "user",
              domain: "example.com",
            },
          ],
          defaultRole: null,
        }),
      );
      const appliedPolicy = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "policy",
        "apply",
        "--file",
        policyPath,
      ]);
      expect(
        accessPolicyApplyResultSchema.parse(
          jsonDocument(appliedPolicy.stdout).data,
        ).policy.bindings,
      ).toHaveLength(2);
      const reconfiguredOidc = await runCli(
        executable,
        [
          "--root",
          root,
          "--json",
          "access",
          "oidc",
          "add",
          "--id",
          "primary",
          "--name",
          "Primary",
          "--issuer",
          "https://identity.example.com/tenant",
          "--client-id",
          "replacement-client",
          "--client-secret-env",
          "TEST_OIDC_SECRET",
          "--origin",
          "https://localbase.example.com",
        ],
        undefined,
        { TEST_OIDC_SECRET: oidcSecret },
      );
      expect(reconfiguredOidc.exitCode).toBe(0);
      expect(
        accessConfigureResultSchema.parse(
          jsonDocument(reconfiguredOidc.stdout).data,
        ).config.permissions,
      ).toEqual(["inference:chat"]);
      const listedOidc = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "oidc",
        "list",
      ]);
      expect(
        accessOidcListResultSchema.parse(jsonDocument(listedOidc.stdout).data)
          .registrations,
      ).toEqual([
        expect.objectContaining({
          id: "primary",
          clientId: "replacement-client",
        }),
      ]);
      const testedPolicy = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "policy",
        "test",
        "--issuer",
        "https://identity.example.com/tenant",
        "--subject",
        "owner",
        "--email",
        "person@example.com",
      ]);
      expect(
        accessPolicyTestResultSchema.parse(
          jsonDocument(testedPolicy.stdout).data,
        ),
      ).toEqual({
        policyConfigured: true,
        matchedRoles: ["admin", "user"],
        permissions: ["inference:chat", "models:manage", "access:manage"],
      });
      await writeFile(
        policyPath,
        JSON.stringify({
          roles: [{ name: "user", permissions: ["inference:chat"] }],
          bindings: [
            {
              kind: "email",
              role: "user",
              email: "person@example.com",
            },
          ],
          defaultRole: null,
        }),
      );
      const rejectedPolicy = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "policy",
        "apply",
        "--file",
        policyPath,
      ]);
      expect(rejectedPolicy.exitCode).not.toBe(0);
      const stillConfigured = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "policy",
        "test",
        "--issuer",
        "https://identity.example.com/tenant",
        "--subject",
        "owner",
      ]);
      expect(
        accessPolicyTestResultSchema.parse(
          jsonDocument(stillConfigured.stdout).data,
        ).matchedRoles,
      ).toEqual(["admin"]);
      const clearedPolicy = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "policy",
        "clear",
      ]);
      expect(
        accessPolicyClearResultSchema.parse(
          jsonDocument(clearedPolicy.stdout).data,
        ),
      ).toEqual({ cleared: true, restartRequired: false });
      const disabledAccess = await runCli(executable, [
        "--root",
        root,
        "--json",
        "access",
        "oidc",
        "remove",
        "primary",
      ]);
      expect(
        accessOidcRemoveResultSchema.parse(
          jsonDocument(disabledAccess.stdout).data,
        ),
      ).toEqual({ removed: true, config: null, restartRequired: true });

      const configuredDatabase = readFileSync(join(root, "local-base.db"));
      const doctor = await runCli(executable, [
        "--root",
        root,
        "doctor",
        "--json",
      ]);
      expect(doctor.exitCode).toBe(0);
      expect(doctor.stdout).not.toContain("private-token");
      expect(jsonDocument(doctor.stdout)).toMatchObject({
        ok: true,
        data: {
          configuration: {
            selectedLlmModels: [
              "qwen2.5-coder-7b-instruct-q4_k_m",
              "mistral-nemo-12b-instruct-q4_k_m",
            ],
            activeLlmModel: "mistral-nemo-12b-instruct-q4_k_m",
            parallel: 3,
            ctxSize: 8192,
            hfTokenConfigured: true,
          },
        },
      });
      expect(readFileSync(join(root, "local-base.db"))).toEqual(
        configuredDatabase,
      );

      const configureKeyRoot = join(directory, "configure-key-data");
      const configuredWithKey = await runCli(executable, [
        "--root",
        configureKeyRoot,
        "--json",
        "configure",
        "--defaults",
        "--create-key",
      ]);
      const configureKeyData = jsonDocument(configuredWithKey.stdout).data as {
        createdKey: { secret: string };
      };
      expect(configureKeyData.createdKey.secret).toMatch(/^lb_/);
      expect(configuredWithKey.stderr).not.toContain(
        configureKeyData.createdKey.secret,
      );

      const catalogRoot = join(directory, "catalog-only-data");
      const catalog = await runCli(executable, [
        "--root",
        catalogRoot,
        "--json",
        "models",
        "catalog",
      ]);
      expect(catalog.exitCode).toBe(0);
      expect(jsonDocument(catalog.stdout)).toMatchObject({ ok: true });
      expect(existsSync(catalogRoot)).toBe(false);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
  { timeout: 30_000 },
);

test(
  "compiled key commands persist scopes, keep secrets private, and reject invalid edits before mutation",
  async () => {
    const directory = mkdtempSync(join(tmpdir(), "local-base-scoped-cli-"));
    const executable = join(directory, "local-base");
    const root = join(directory, "data");
    const invoke = (...args: string[]) =>
      runCli(executable, ["--root", root, "--json", "keys", ...args]);
    try {
      await compileCli(executable);
      const invalidCreate = await invoke("create", "--scopes", "*");
      expect(invalidCreate.exitCode).toBe(2);
      expect(jsonDocument(invalidCreate.stdout)).toMatchObject({
        ok: false,
        error: { code: "invalid_input" },
      });
      expect(existsSync(root)).toBe(false);

      const created = await invoke(
        "create",
        "--name",
        "limited",
        "--expires-days",
        "30",
        "--scopes",
        " models:read, inference:video,models:read ",
      );
      expect(created.exitCode).toBe(0);
      const initial = keySecretResultSchema.parse(
        jsonDocument(created.stdout).data,
      );
      expect(initial.key.scopes).toEqual(["inference:video", "models:read"]);
      expect(created.stderr).not.toContain(initial.secret);
      expect(created.stdout.split(initial.secret)).toHaveLength(2);

      const stored = readFileSync(join(root, "local-base.db"));
      for (const args of [
        ["scopes", initial.key.id],
        ["scopes", initial.key.id, "--scopes"],
        ["scopes", initial.key.id, "--scopes", "--non-interactive"],
        ["scopes", initial.key.id, "--scopes", "--"],
        ["scopes", initial.key.id, "--scopes", "models:read,"],
        ["scopes", initial.key.id, "--scopes", "models:write"],
        ["scopes", initial.key.id, "--scopes", "inference:*"],
        ["create", "--scopes", "models:read,typo"],
        ["create", "--scopes"],
        ["create", "--scopes", "--name", "missing-scope-value"],
      ]) {
        const invalid = await invoke(...args);
        expect(invalid.exitCode).toBe(2);
        expect(jsonDocument(invalid.stdout)).toMatchObject({
          ok: false,
          error: { code: "invalid_input" },
        });
        expect(readFileSync(join(root, "local-base.db"))).toEqual(stored);
      }

      const edited = await invoke(
        "scopes",
        initial.key.id,
        "--scopes",
        [...permissionSchema.options].reverse().join(","),
      );
      expect(edited.exitCode).toBe(0);
      const editedData = keyMetadataResultSchema.parse(
        jsonDocument(edited.stdout).data,
      );
      expect(editedData.key).toEqual({
        ...initial.key,
        scopes: permissionSchema.options,
      });
      expect(edited.stdout + edited.stderr).not.toContain(initial.secret);

      const rotated = await invoke("rotate", initial.key.id);
      expect(rotated.exitCode).toBe(0);
      const rotation = keySecretResultSchema.parse(
        jsonDocument(rotated.stdout).data,
      );
      expect(rotation.secret).not.toBe(initial.secret);
      expect(rotation.key).toMatchObject({
        id: initial.key.id,
        expiresAt: initial.key.expiresAt,
        scopes: permissionSchema.options,
      });
      expect(rotated.stderr).not.toContain(rotation.secret);
      expect(rotated.stdout.split(rotation.secret)).toHaveLength(2);

      const revoked = await invoke("revoke", initial.key.id);
      expect(revoked.exitCode).toBe(0);
      const revocation = keyMetadataResultSchema.parse(
        jsonDocument(revoked.stdout).data,
      );
      expect(revocation.key).toEqual({
        ...rotation.key,
        revokedAt: expect.any(String),
      });
      const cleared = await invoke("scopes", initial.key.id, "--scopes", "");
      expect(cleared.exitCode).toBe(0);
      expect(
        keyMetadataResultSchema.parse(jsonDocument(cleared.stdout).data).key,
      ).toEqual({ ...revocation.key, scopes: [] });

      const listed = await invoke("list");
      expect(listed.exitCode).toBe(0);
      expect(
        keysListResultSchema.parse(jsonDocument(listed.stdout).data).keys,
      ).toEqual([{ ...revocation.key, scopes: [] }]);
      const humanList = await runCli(executable, [
        "--root",
        root,
        "keys",
        "list",
        "--non-interactive",
      ]);
      expect(humanList.exitCode).toBe(0);
      expect(humanList.stdout).toContain("scopes=");
      for (const result of [revoked, cleared, listed, humanList]) {
        expect(result.stdout + result.stderr).not.toContain(initial.secret);
        expect(result.stdout + result.stderr).not.toContain(rotation.secret);
        expect(result.stdout + result.stderr).not.toContain("keyHash");
      }

      const empty = await invoke("create", "--scopes", "");
      expect(empty.exitCode).toBe(0);
      expect(
        keySecretResultSchema.parse(jsonDocument(empty.stdout).data).key.scopes,
      ).toEqual([]);
      const missing = await invoke(
        "scopes",
        "missing-key",
        "--scopes",
        "models:read",
      );
      expect(missing.exitCode).toBe(1);
      expect(jsonDocument(missing.stdout)).toMatchObject({
        ok: false,
        error: { message: "API key not found: missing-key" },
      });
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
  { timeout: 30_000 },
);

test("JSON mode rejects interactive and destructive commands without consent", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-json-input-"));
  const executable = join(directory, "local-base");
  const root = join(directory, "data");

  try {
    await compileCli(executable);

    const interactive = await runCli(executable, [
      "--json",
      "configure",
      "--all",
    ]);
    expect(interactive.exitCode).toBe(2);
    expect(jsonDocument(interactive.stdout)).toMatchObject({
      ok: false,
      error: { code: "invalid_input" },
    });

    const malformedGlobal = await runCli(executable, ["--json", "--root"]);
    expect(malformedGlobal.exitCode).toBe(2);
    expect(jsonDocument(malformedGlobal.stdout)).toMatchObject({
      ok: false,
      error: { code: "invalid_input" },
    });

    const reset = await runCli(executable, ["--root", root, "reset", "--json"]);
    expect(reset.exitCode).toBe(2);
    expect(jsonDocument(reset.stdout)).toMatchObject({
      ok: false,
      error: { code: "invalid_input" },
    });
    expect(reset.stderr).toContain("--yes");

    const literal = await runCli(executable, [
      "--root",
      root,
      "configure",
      "--json",
      "--defaults",
      "--no-create-key",
    ]);
    expect(literal.exitCode).toBe(0);

    const invalidToml = join(directory, "invalid-model.toml");
    await Bun.write(invalidToml, 'selectedLlmModels = ["unknown"]\n');
    for (const invocation of [
      [
        "--root",
        root,
        "configure",
        "--json",
        "--defaults",
        "--llm-models",
        "unknown",
      ],
      ["--root", root, "configure", "--json", "--config", invalidToml],
      ["--root", root, "models", "install", "unknown", "--json"],
    ]) {
      const invalidCatalogModel = await runCli(executable, invocation);
      expect(invalidCatalogModel.exitCode).toBe(2);
      expect(jsonDocument(invalidCatalogModel.stdout)).toMatchObject({
        ok: false,
        error: { code: "invalid_input" },
      });
    }

    const invalidModel = await runCli(executable, [
      "--root",
      root,
      "--json",
      "configure",
      "--active-llm",
      "unknown-model",
    ]);
    expect(invalidModel.exitCode).toBe(2);
    expect(jsonDocument(invalidModel.stdout)).toMatchObject({
      ok: false,
      error: { code: "invalid_input" },
    });

    const incompatibleSelection = await runCli(executable, [
      "--root",
      root,
      "--json",
      "configure",
      "--defaults",
      "--no-create-key",
      "--llm-models",
      "qwen2.5-coder-7b-instruct-q4_k_m",
      "--active-llm",
      "mistral-nemo-12b-instruct-q4_k_m",
    ]);
    expect(incompatibleSelection.exitCode).toBe(2);
    expect(jsonDocument(incompatibleSelection.stdout)).toMatchObject({
      ok: false,
      error: { code: "invalid_input" },
    });

    const disabledServe = await runCli(executable, [
      "--root",
      root,
      "serve",
      "--json",
      "--no-llm",
      "--no-stt",
      "--no-image",
      "--no-auth",
    ]);
    expect(disabledServe.exitCode).toBe(2);
    expect(jsonDocument(disabledServe.stdout)).toMatchObject({
      event: "error",
      error: { code: "invalid_input" },
    });
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test(
  "compiled declarative config commands support files, stdin, strict errors, deterministic JSON, and re-apply",
  async () => {
    const directory = mkdtempSync(join(tmpdir(), "local-base-config-cli-"));
    const executable = join(directory, "local-base");
    const root = join(directory, "data");
    const desired = configurationDocument(defaultConfig(root));
    const source = renderConfiguration(desired);
    const file = join(directory, "desired.toml");
    const invoke = (args: string[], stdin?: string) =>
      runCli(executable, ["--root", root, "config", ...args], stdin);
    try {
      await compileCli(executable);
      await Bun.write(file, source);
      const valid = await invoke(["validate", "--file", file, "--json"]);
      expect(valid.exitCode, valid.stderr).toBe(0);
      expect(jsonDocument(valid.stdout)).toMatchObject({
        ok: true,
        data: { valid: true, configuration: desired },
      });
      expect(
        (await invoke(["validate", "--file", "-", "--json"], source)).stdout,
      ).toBe(valid.stdout);
      expect(
        (await invoke(["validate", "--file=-", "--json"], source)).stdout,
      ).toBe(valid.stdout);
      const planned = await invoke(
        ["plan", "--file", "-", "--json", "--detailed-exit-code"],
        source,
      );
      expect(planned.exitCode).toBe(2);
      expect(jsonDocument(planned.stdout)).toMatchObject({
        ok: true,
        data: { changed: true },
      });
      expect((await invoke(["plan", "--file", file, "--json"])).exitCode).toBe(
        0,
      );
      expect(existsSync(root)).toBe(false);
      for (const args of [
        ["plan"],
        ["plan", "--file"],
        ["plan", "--bogus"],
        ["plan", "--root", ""],
        ["plan", "--file", "missing-file"],
        ["apply", "--file", file, "--restart", "sometimes"],
      ]) {
        const invalid = await invoke([...args, "--json"]);
        expect(invalid.exitCode).toBe(1);
        expect(jsonDocument(invalid.stdout).ok).toBe(false);
      }
      const invalidSource = await invoke(
        ["plan", "--file", "-", "--json", "--detailed-exit-code"],
        "version = 2",
      );
      expect(invalidSource.exitCode).toBe(1);
      expect(jsonDocument(invalidSource.stdout)).toMatchObject({
        ok: false,
        error: { code: "invalid_input" },
      });
      expect(existsSync(root)).toBe(false);
      const applied = await invoke(
        ["apply", "--file", "-", "--restart", "never", "--json"],
        source,
      );
      expect(applied.exitCode, applied.stderr).toBe(0);
      expect(jsonDocument(applied.stdout)).toMatchObject({
        ok: true,
        data: { changed: true, activation: "restart-required" },
      });
      const pendingPlan = await invoke([
        "plan",
        "--file",
        file,
        "--json",
        "--detailed-exit-code",
      ]);
      expect(pendingPlan.exitCode).toBe(2);
      expect(jsonDocument(pendingPlan.stdout)).toMatchObject({
        data: { changed: false, pendingRestart: true, restartRequired: true },
      });
      const pendingShow = await invoke(["show", "--json"]);
      expect(jsonDocument(pendingShow.stdout)).toMatchObject({
        data: { pendingRestart: true },
      });
      const pendingHuman = await invoke(["show"]);
      expect(parseConfiguration(pendingHuman.stdout)).toEqual(desired);
      expect(pendingHuman.stderr).toContain("Pending restart");
      const repeated = await invoke([
        "apply",
        "--file",
        file,
        "--restart",
        "never",
        "--json",
      ]);
      expect(jsonDocument(repeated.stdout)).toMatchObject({
        data: { changed: false, pendingRestart: true },
      });
      const database = new DatabaseSession();
      try {
        acknowledgeStaticConfiguration(database, root, defaultConfig(root));
      } finally {
        database.close();
      }
      const unchanged = await invoke(["apply", "--file", file, "--json"]);
      expect(unchanged.exitCode, unchanged.stderr).toBe(0);
      expect(jsonDocument(unchanged.stdout)).toMatchObject({
        ok: true,
        data: { changed: false, changes: [], activation: "unchanged" },
      });
      const stable = readFileSync(join(root, "local-base.db"));
      const noChange = await invoke([
        "plan",
        "--file",
        file,
        "--detailed-exit-code",
        "--json",
      ]);
      expect(noChange.exitCode).toBe(0);
      expect(
        (
          await invoke([
            "plan",
            "--file",
            file,
            "--detailed-exit-code",
            "--json",
          ])
        ).stdout,
      ).toBe(noChange.stdout);
      const shown = await invoke(["show"]);
      expect(shown.exitCode, shown.stderr).toBe(0);
      expect(parseConfiguration(shown.stdout)).toEqual(desired);
      expect(
        jsonDocument((await invoke(["show", "--json"])).stdout),
      ).toMatchObject({ ok: true, data: { document: shown.stdout } });
      expect(
        (await invoke(["apply", "--file", "-", "--json"], shown.stdout)).stdout,
      ).toBe(unchanged.stdout);
      expect(readFileSync(join(root, "local-base.db"))).toEqual(stable);
      desired.runtime.ctxSize = 8192;
      const changedSource = renderConfiguration(desired);
      const diff = await invoke(
        ["plan", "--file", "-", "--detailed-exit-code", "--json"],
        changedSource,
      );
      expect(diff.exitCode).toBe(2);
      expect(jsonDocument(diff.stdout)).toMatchObject({
        data: {
          restartRequired: false,
          changes: [
            {
              path: "runtime.ctxSize",
              before: 131072,
              after: 8192,
              activation: "hot",
            },
          ],
        },
      });
      const human = await invoke(["plan", "--file", "-"], changedSource);
      expect(human.stdout).toBe("runtime.ctxSize: 131072 -> 8192 [hot]\n");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
  { timeout: 30_000 },
);
