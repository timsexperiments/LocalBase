import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const projectRoot = join(import.meta.dirname, "../../../..");

type CliResult = { exitCode: number; stdout: string; stderr: string };

async function compileCli(outputPath: string): Promise<void> {
  const build = Bun.spawn(
    [
      process.execPath,
      "build",
      "src/cli.ts",
      "--compile",
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

async function runCli(executable: string, args: string[]): Promise<CliResult> {
  const process = Bun.spawn([executable, ...args], {
    cwd: projectRoot,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    process.exited,
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
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

      const prompt = "Private prompt content";
      const updated = await runCli(executable, [
        "--root",
        root,
        "--json",
        "prompt",
        "set",
        prompt,
      ]);
      expect(updated.exitCode).toBe(0);
      expect(updated.stdout).not.toContain(prompt);

      const doctor = await runCli(executable, [
        "--root",
        root,
        "doctor",
        "--json",
      ]);
      expect(doctor.exitCode).toBe(0);
      expect(doctor.stdout).not.toContain("private-token");
      expect(doctor.stdout).not.toContain(prompt);
      expect(doctor.stdout).not.toContain("systemPrompt");

      const shownPrompt = await runCli(executable, [
        "--root",
        root,
        "prompt",
        "show",
        "--json",
      ]);
      expect(jsonDocument(shownPrompt.stdout).data).toMatchObject({
        prompt,
        source: "custom",
      });

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
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
  { timeout: 30_000 },
);

test("JSON mode rejects prompts and destructive commands without consent", async () => {
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
