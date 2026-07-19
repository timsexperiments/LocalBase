import { expect, test, describe, beforeAll, afterAll } from "bun:test";
import { type LocalBaseConfig, saveConfig, defaultConfig } from "../../manager";
import { join } from "node:path";
import { mkdirSync, rmSync } from "node:fs";

const TEST_ROOT = join(import.meta.dirname, "../../../test-runtime-root");

describe("API Gateway Integration & Schema Validation", () => {
  let config: LocalBaseConfig;
  let serverProcess: any;

  beforeAll(async () => {
    rmSync(TEST_ROOT, { recursive: true, force: true });
    mkdirSync(TEST_ROOT, { recursive: true });

    // Initialize the storage root and SQLite database
    Bun.spawnSync(["bun", "run", "src/cli.ts", "init", "--root", TEST_ROOT]);

    // Generate test database config
    config = defaultConfig(TEST_ROOT);
    config.port = 18001; // Mock llama-server port
    config.sttPort = 18002;
    config.activeLlmModel = "qwen2.5-coder-1.5b-instruct-q4_k_m";
    config.selectedLlmModels = ["qwen2.5-coder-1.5b-instruct-q4_k_m"];
    config.activeSttModel = "whisper-large-v3-turbo";
    config.selectedSttModels = ["whisper-large-v3-turbo"];
    config.activeImageModel = "stable-diffusion-v1-5";
    config.selectedImageModels = ["stable-diffusion-v1-5"];
    saveConfig(config);

    // Create dummy mock binaries to bypass ensureBinary downloads
    const binDir = join(TEST_ROOT, "bin");
    mkdirSync(binDir, { recursive: true });
    const dummyScript = "#!/bin/sh\nexit 0\n";
    await Bun.write(join(binDir, "llama-server"), dummyScript);
    await Bun.write(join(binDir, "whisper-server"), dummyScript);
    await Bun.write(join(binDir, "sd-server"), dummyScript);
    Bun.spawnSync(["chmod", "+x", join(binDir, "llama-server")]);
    Bun.spawnSync(["chmod", "+x", join(binDir, "whisper-server")]);
    Bun.spawnSync(["chmod", "+x", join(binDir, "sd-server")]);

    // Create dummy model files to bypass serve startup downloader checks
    const llmDir = join(TEST_ROOT, "models", "llm");
    mkdirSync(llmDir, { recursive: true });
    await Bun.write(
      join(llmDir, "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf"),
      "dummy llm model",
    );

    const sttDir = join(TEST_ROOT, "models", "stt");
    mkdirSync(sttDir, { recursive: true });
    await Bun.write(join(sttDir, "ggml-large-v3-turbo.bin"), "dummy stt model");

    const imageDir = join(TEST_ROOT, "models", "image");
    mkdirSync(imageDir, { recursive: true });
    await Bun.write(
      join(imageDir, "v1-5-pruned-emaonly.safetensors"),
      "dummy image model",
    );

    // Spawn serving gateway wrapper on port 8989
    serverProcess = Bun.spawn(
      [
        "bun",
        "run",
        "src/cli.ts",
        "serve",
        "--root",
        TEST_ROOT,
        "--port",
        "8989",
        "--llm",
        "true",
        "--stt",
        "true",
        "--image",
        "true",
        "--auth",
        "false", // Disable auth for easy base testing
      ],
      {
        stdout: "pipe",
        stderr: "pipe",
      },
    );

    // Wait until server is healthy
    const maxRetries = 20;
    for (let i = 0; i < maxRetries; i++) {
      try {
        const res = await fetch("http://localhost:8989/health");
        if (res.ok) {
          break;
        }
      } catch (e) {}
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  });

  afterAll(() => {
    if (serverProcess) {
      serverProcess.kill();
    }
    rmSync(TEST_ROOT, { recursive: true, force: true });
  });

  test("GET /health status response", async () => {
    const res = await fetch("http://localhost:8989/health");
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.status).toBe("ok");
    expect(body.enabled.llm).toBe(true);
    expect(body.enabled.stt).toBe(true);
    expect(body.enabled.image).toBe(true);
  });

  test("GET /v1/models lists configured models", async () => {
    const res = await fetch("http://localhost:8989/v1/models");
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.object).toBe("list");
    expect(body.data.length).toBeGreaterThan(0);
    expect(body.data[0].id).toBe("qwen2.5-coder-1.5b-instruct-q4_k_m");
  });

  test("POST /v1/chat/completions fails validation on missing messages", async () => {
    const res = await fetch("http://localhost:8989/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
      }),
    });

    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error.type).toBe("invalid_request_error");
    expect(body.error.code).toBe("validation_failed");
    expect(body.error.message).toContain("messages: ");
  });

  test("POST /v1/chat/completions fails validation on invalid message format", async () => {
    const res = await fetch("http://localhost:8989/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: "qwen2.5-coder-1.5b-instruct-q4_k_m",
        messages: [{ role: "invalid-role", content: "hello" }],
      }),
    });

    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error.message).toContain("messages.0.role: ");
  });

  test("POST /v1/images/generations fails validation on missing prompt", async () => {
    const res = await fetch("http://localhost:8989/v1/images/generations", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        n: 1,
      }),
    });

    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error.message).toContain("prompt: ");
  });

  test("POST /v1/embeddings fails validation on missing input", async () => {
    const res = await fetch("http://localhost:8989/v1/embeddings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: "embeddings-model",
      }),
    });

    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error.message).toContain("input: ");
  });

  test("POST /v1/audio/transcriptions fails validation on missing file", async () => {
    const fd = new FormData();
    fd.append("model", "whisper-large-v3");

    const res = await fetch("http://localhost:8989/v1/audio/transcriptions", {
      method: "POST",
      body: fd,
    });

    expect(res.status).toBe(400);
    const body = await res.json();
    expect(body.error.message).toContain("file: ");
  });
});
