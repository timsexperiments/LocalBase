import { existsSync } from "node:fs";
import { join, basename } from "node:path";
import {
  type LocalBaseConfig,
  startLlamaServerProcess,
  startWhisperServerProcess,
  startSdServerProcess,
  validateApiKey,
  installModel,
  saveConfig,
} from "../../../manager";
import {
  byId,
  evaluateModelFit,
  calculateMaxSafeContextSize,
} from "../../../catalog";
import type { AppContext } from "../../../context";
import { parseBool, parseFlag, toInt } from "../../../utils/args";
import { syncContinueConfig } from "../../config/commands/configure";
import { type ILogger } from "../../../utils/logger";
import { DEFAULT_SYSTEM_PROMPT } from "./prompt";

type AuthMode = "bearer" | "x-api-key" | "either";

type ModalityState = {
  llm: boolean;
  stt: boolean;
  image: boolean;
};

function parseAuthMode(raw: string | undefined): AuthMode {
  if (!raw) return "either";
  if (raw === "bearer" || raw === "x-api-key" || raw === "either") return raw;
  throw new Error(
    `Invalid --auth-mode value: ${raw}. Expected bearer|x-api-key|either`,
  );
}

function printUnifiedNextSteps(
  host: string,
  port: number,
  llmPort: number,
  sttPort: number,
  imagePort: number,
  authRequired: boolean,
  authMode: AuthMode,
  enabled: ModalityState,
): void {
  console.log("\nUnified API wrapper started.");
  console.log(`Wrapper base URL: http://${host}:${port}`);
  if (enabled.llm)
    console.log(`OpenAI-compatible LLM endpoint: http://${host}:${port}/v1`);
  if (enabled.stt)
    console.log(
      `OpenAI-compatible STT endpoint: http://${host}:${port}/v1/audio/transcriptions`,
    );
  if (enabled.image)
    console.log(
      `OpenAI-compatible Image endpoint: http://${host}:${port}/v1/images/generations`,
    );
  if (authRequired) {
    console.log(`Authentication: enabled (mode=${authMode}).`);
    console.log(
      "Supported credentials: Authorization: Bearer <key>, x-api-key: <key> (mode-dependent).",
    );
  } else {
    console.log("Authentication: disabled via --auth false.");
  }
  console.log(
    `Enabled modalities: ${
      Object.entries(enabled)
        .filter(([, on]) => on)
        .map(([k]) => k)
        .join(", ") || "none"
    }`,
  );
  if (enabled.llm)
    console.log(`Upstream llama-server: http://127.0.0.1:${llmPort}`);
  if (enabled.stt)
    console.log(`Upstream whisper-server: http://127.0.0.1:${sttPort}`);
  if (enabled.image)
    console.log(`Upstream sd-server: http://127.0.0.1:${imagePort}`);

  if (enabled.llm) {
    console.log("\nExample chat request (Bearer):");
    console.log(
      `curl http://${host}:${port}/v1/chat/completions -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"<your-model>","messages":[{"role":"user","content":"hello"}]}'`,
    );
  }
  if (enabled.stt) {
    console.log("\nExample STT request (x-api-key):");
    console.log(
      `curl -X POST http://${host}:${port}/v1/audio/transcriptions -H 'x-api-key: <API_KEY>' -F file=@audio.wav -F model=whisper`,
    );
  }
  if (enabled.image) {
    console.log("\nExample Image request (Bearer):");
    console.log(
      `curl http://${host}:${port}/v1/images/generations -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"prompt":"A scenic sunset","n":1,"size":"512x512"}'`,
    );
  }
}

function extractBearerToken(request: Request): string | null {
  const auth =
    request.headers.get("authorization") ??
    request.headers.get("Authorization");
  if (!auth) return null;
  const [scheme, token] = auth.split(/\s+/, 2);
  if (!scheme || !token) return null;
  if (scheme.toLowerCase() !== "bearer") return null;
  return token.trim();
}

function extractApiKeyHeader(request: Request): string | null {
  return request.headers.get("x-api-key") ?? request.headers.get("X-API-Key");
}

function extractAuthToken(request: Request, mode: AuthMode): string | null {
  if (mode === "bearer") return extractBearerToken(request);
  if (mode === "x-api-key") return extractApiKeyHeader(request)?.trim() ?? null;
  return (
    extractBearerToken(request) ?? extractApiKeyHeader(request)?.trim() ?? null
  );
}

function unauthorized(mode: AuthMode): Response {
  const hint = mode === "x-api-key" ? "x-api-key" : "Bearer";
  return Response.json(
    {
      error: "Unauthorized",
      expected: mode === "either" ? "Bearer or x-api-key" : hint,
    },
    { status: 401, headers: { "www-authenticate": "Bearer" } },
  );
}

function notConfigured(feature: string): Response {
  return Response.json(
    {
      error: `${feature} route is disabled`,
      hint: `Set --${feature.toLowerCase()} true and configure upstream/model to enable this route`,
    },
    { status: 501 },
  );
}

async function proxyRequest(
  request: Request,
  targetBase: string,
  pathOverride?: string,
): Promise<Response> {
  const incoming = new URL(request.url);
  const path = pathOverride ?? incoming.pathname;
  const target = `${targetBase}${path}${incoming.search}`;
  const upstream = await fetch(target, {
    method: request.method,
    headers: request.headers,
    body: request.body,
  });

  return new Response(upstream.body, {
    status: upstream.status,
    headers: upstream.headers,
  });
}

/**
 * Supervisor that manages a model backend subprocess (e.g. llama-server).
 * Handles lazy loading, auto-restart crash recovery with exponential backoff,
 * sliding-window crash limits, stdout/stderr log piping, and startup readiness checks.
 */
class ManagedService {
  private proc: Bun.Subprocess | null = null;
  private name: string;
  private startFn: () => Promise<Bun.Subprocess>;
  private healthUrl: string;
  private logger: ILogger;
  private crashTimes: number[] = [];
  private isRestarting = false;
  private restartPromise: Promise<void> | null = null;

  constructor(
    name: string,
    healthUrl: string,
    logger: ILogger,
    startFn: () => Promise<Bun.Subprocess>,
  ) {
    this.name = name;
    this.healthUrl = healthUrl;
    this.logger = logger;
    this.startFn = startFn;
  }

  /**
   * Lazily starts the service on first use, or awaits recovery if currently restarting.
   */
  async ensureRunning(): Promise<void> {
    if (this.proc && this.proc.exitCode === null) {
      return;
    }
    if (this.isRestarting) {
      await this.restartPromise;
      return;
    }
    await this.start();
  }

  /**
   * Spawns the subprocess, registers crash handlers, pipes logs, and awaits healthy status.
   * Employs exponential backoff on retry and exits the manager if crash limits are hit.
   */
  private async start(): Promise<void> {
    this.isRestarting = true;
    this.restartPromise = (async () => {
      const now = Date.now();
      this.crashTimes = this.crashTimes.filter((t) => now - t < 300000); // 5 min window
      const crashCount = this.crashTimes.length;

      if (crashCount >= 5) {
        this.logger.error(
          this.name,
          `Service has crashed ${crashCount} times in 5 minutes. Stopping manager.`,
        );
        process.exit(1);
      }

      if (crashCount > 0) {
        const backoffMs = Math.min(1000 * Math.pow(2, crashCount - 1), 16000);
        this.logger.warn(
          this.name,
          `Crashed previously. Backing off for ${backoffMs}ms before restarting...`,
        );
        await new Promise((resolve) => setTimeout(resolve, backoffMs));
      }

      this.logger.info(this.name, "Starting subprocess...");
      try {
        this.proc = await this.startFn();
        if (this.proc.stdout && typeof this.proc.stdout !== "number")
          this.logger.pipeStream(this.proc.stdout, this.name);
        if (this.proc.stderr && typeof this.proc.stderr !== "number")
          this.logger.pipeStream(this.proc.stderr, this.name);

        this.proc.exited.then(() => {
          this.handleCrash();
        });

        const ok = await this.waitHealthy();
        if (!ok) {
          throw new Error("Health check failed to pass within timeout");
        }
        this.logger.info(this.name, "Subprocess is healthy and ready.");
      } catch (err) {
        this.logger.error(this.name, "Failed to start service", err as Error);
        this.crashTimes.push(Date.now());
        this.proc?.kill();
        this.proc = null;
        this.isRestarting = false;
        throw err;
      }
      this.isRestarting = false;
    })();
    await this.restartPromise;
  }

  /**
   * Polls the backend's /health endpoint until it is online or startup times out.
   */
  private async waitHealthy(): Promise<boolean> {
    const timeout = 30000; // 30s
    const interval = 200; // 200ms
    const start = Date.now();

    while (Date.now() - start < timeout) {
      if (this.proc?.exitCode !== null) {
        this.logger.error(
          this.name,
          `Subprocess exited during startup with code ${this.proc?.exitCode}`,
        );
        return false;
      }
      try {
        const res = await fetch(this.healthUrl);
        if (res.ok) return true;
      } catch (e) {
        // Expected network failures while booting
      }
      await new Promise((resolve) => setTimeout(resolve, interval));
    }
    return false;
  }

  /**
   * Forcefully kills the subprocess.
   */
  kill(): void {
    if (this.proc) {
      this.proc.kill();
      this.proc = null;
    }
  }

  /**
   * Initiates asynchronous process recovery when a running subprocess exits.
   */
  handleCrash(): void {
    if (this.proc && this.proc.exitCode !== null && !this.isRestarting) {
      this.logger.warn(
        this.name,
        `Subprocess exited unexpectedly with code ${this.proc.exitCode}. Triggering self-healing...`,
      );
      this.crashTimes.push(Date.now());
      this.proc = null;
      this.start().catch(() => {});
    }
  }
}

/**
 * Returns a standard HTTP 503 service unavailable response.
 */
function serviceUnavailable(serviceName: string): Response {
  return new Response(
    JSON.stringify({
      error: `${serviceName} service is currently restarting or unavailable. Please try again shortly.`,
    }),
    {
      status: 503,
      headers: {
        "Content-Type": "application/json",
        "Retry-After": "5",
      },
    },
  );
}

/**
 * Main command handler for 'serve'. Starts the unified proxy server.
 * Handles dynamic context sizing: min(recommendedForHardwareAndModel, maxContextCeiling).
 * Automatically maps OpenAI 'developer' role to 'system' role for tokenizer compatibility.
 * Maps client-side '/v1/slots|metrics|props|system_info' queries to standard llama-server root endpoints.
 * Automatically synchronizes active model specifications and context limits to OpenCode in real-time.
 */
export async function runServe(
  args: string[],
  ctx: AppContext,
): Promise<number> {
  const config = ctx.config;
  const wrapperHost = parseFlag(args, "--host") ?? "0.0.0.0";
  const wrapperPort = toInt(parseFlag(args, "--port"), 2273);

  const llmHost = parseFlag(args, "--llm-host") ?? "127.0.0.1";
  const llmPort = toInt(parseFlag(args, "--llm-port"), config.port);
  const sttHost = parseFlag(args, "--stt-host") ?? "127.0.0.1";
  const sttPort = toInt(parseFlag(args, "--stt-port"), config.sttPort);
  const imageHost = parseFlag(args, "--image-host") ?? "127.0.0.1";
  const imagePort = toInt(parseFlag(args, "--image-port"), 8090);

  let ctxSize = toInt(parseFlag(args, "--ctx-size"), 0);
  if (!ctxSize) {
    const spec = byId(config.activeLlmModel);
    const recommendedCtx = spec
      ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
      : ctx.specs.gpuVramGb >= 32
        ? 32768
        : 8192;
    ctxSize = Math.min(recommendedCtx, config.ctxSize);
    console.log(`\n💡 Dynamic Context Size: Initialized to ${ctxSize} tokens.`);
    console.log(
      `   (Calculated best for "${config.activeLlmModel}" on ${ctx.specs.gpuVramGb} GB hardware: ${recommendedCtx} tokens, limited by max context setting: ${config.ctxSize} tokens)`,
    );
  } else {
    console.log(
      `\n💡 Context Size: Using explicit override --ctx-size ${ctxSize} tokens.`,
    );
  }

  // Automatically synchronize active model and calculated context size with Continue configuration
  await syncContinueConfig(config, ctxSize);
  const sttPath = parseFlag(args, "--stt-path") ?? "/inference";
  const authRequired = parseFlag(args, "--auth") !== "false";
  const authMode = parseAuthMode(parseFlag(args, "--auth-mode"));

  let llmModelFile = parseFlag(args, "--llm-model-file");
  if (!llmModelFile) {
    const spec = byId(config.activeLlmModel);
    if (
      spec?.filename &&
      existsSync(join(config.llmModelsDir, spec.filename))
    ) {
      llmModelFile = spec.filename;
    } else if (
      existsSync(join(config.llmModelsDir, `${config.activeLlmModel}.bin`))
    ) {
      llmModelFile = `${config.activeLlmModel}.bin`;
    } else {
      llmModelFile = `${config.activeLlmModel}.gguf`;
    }
  }

  let sttModelFile = parseFlag(args, "--stt-model-file");
  if (!sttModelFile) {
    const spec = byId(config.activeSttModel);
    if (
      spec?.filename &&
      existsSync(join(config.sttModelsDir, spec.filename))
    ) {
      sttModelFile = spec.filename;
    } else if (
      existsSync(join(config.sttModelsDir, `${config.activeSttModel}.bin`))
    ) {
      sttModelFile = `${config.activeSttModel}.bin`;
    } else {
      sttModelFile = `${config.activeSttModel}.gguf`;
    }
  }

  let imageModelFile = parseFlag(args, "--image-model-file");
  if (!imageModelFile) {
    const spec = byId(config.activeImageModel);
    if (
      spec?.filename &&
      existsSync(join(config.imageModelsDir, spec.filename))
    ) {
      imageModelFile = spec.filename;
    } else {
      imageModelFile = `${config.activeImageModel}.safetensors`;
    }
  }

  let llmModelExists = existsSync(join(config.llmModelsDir, llmModelFile));
  let sttModelExists = existsSync(join(config.sttModelsDir, sttModelFile));
  let imageModelExists = existsSync(
    join(config.imageModelsDir, imageModelFile),
  );

  const enabled: ModalityState = {
    llm: parseBool(parseFlag(args, "--llm"), true),
    stt: parseBool(
      parseFlag(args, "--stt"),
      config.selectedSttModels.length > 0,
    ),
    image: parseBool(
      parseFlag(args, "--image"),
      config.selectedImageModels.length > 0,
    ),
  };

  if (enabled.stt && !config.activeSttModel) {
    throw new Error(
      "STT modality is enabled but no active STT model is configured. Run `local-base configure` first.",
    );
  }
  if (enabled.image && !config.activeImageModel) {
    throw new Error(
      "Image modality is enabled but no active Image model is configured. Run `local-base configure` first.",
    );
  }

  // Perform memory fit evaluation BEFORE downloading
  const specs = ctx.specs;
  const getModelIdFromFile = (filename: string): string => {
    return filename.replace(/\.(gguf|bin|onnx|safetensors|pth)$/i, "");
  };

  const bypassCheck =
    args.includes("--bypass-memory-check") || args.includes("--force");

  if (enabled.llm) {
    const llmModelId = getModelIdFromFile(llmModelFile);
    const llmSpec = byId(llmModelId);
    if (llmSpec) {
      const fit = evaluateModelFit(llmSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run LLM "${llmSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.error(
          `   Running this model will likely crash the system or cause severe slowdowns.`,
        );
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for LLM "${llmSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.warn(
          `   Leaves only ${fit.headroomGb.toFixed(1)} GB headroom. Large context windows may cause slowdowns.`,
        );
      } else {
        console.log(
          `\n✅ Memory check passed: LLM "${llmSpec.modelId}" fits comfortably in ${specs.gpuVramGb} GB.`,
        );
      }

      const maxSafeCtx = calculateMaxSafeContextSize(llmSpec, specs.gpuVramGb);
      if (ctxSize < maxSafeCtx) {
        console.log(
          `\n💡 Tip: Your system supports a larger context size of up to ${maxSafeCtx} tokens for "${llmSpec.modelId}".`,
        );
        console.log(
          `   You can configure this by running 'local-base configure' or starting with '--ctx-size ${maxSafeCtx}'.`,
        );
      }
    }
  }

  if (enabled.stt) {
    const sttModelId = getModelIdFromFile(sttModelFile);
    const sttSpec = byId(sttModelId);
    if (sttSpec) {
      const fit = evaluateModelFit(sttSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run STT "${sttSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for STT "${sttSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  if (enabled.image) {
    const imageModelId = getModelIdFromFile(imageModelFile);
    const imageSpec = byId(imageModelId);
    if (imageSpec) {
      const fit = evaluateModelFit(imageSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(
          `\n❌ ERROR: Insufficient VRAM/Unified Memory to run Image model "${imageSpec.modelId}".`,
        );
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        if (!bypassCheck) {
          console.error(
            `   To force launch this model anyway, use --bypass-memory-check`,
          );
          return 1;
        } else {
          console.warn(
            `   Bypassing memory validation check and proceeding...`,
          );
        }
      } else if (fit.status === "tight") {
        console.warn(
          `\n⚠️ WARNING: Tight memory fit for Image model "${imageSpec.modelId}".`,
        );
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  // Automatically download models if they pass memory checks and are missing
  if (enabled.llm && !llmModelExists) {
    console.log(
      `LLM model file is missing. Automatically installing "${config.activeLlmModel}"...`,
    );
    const installedPath = await installModel(config, config.activeLlmModel);
    llmModelFile = basename(installedPath);
    llmModelExists = true;
  }

  if (enabled.stt && !sttModelExists) {
    console.log(
      `STT model file is missing. Automatically installing "${config.activeSttModel}"...`,
    );
    const installedPath = await installModel(config, config.activeSttModel);
    sttModelFile = basename(installedPath);
    sttModelExists = true;
  }

  if (enabled.image && !imageModelExists) {
    console.log(
      `Image model file is missing. Automatically installing "${config.activeImageModel}"...`,
    );
    const installedPath = await installModel(config, config.activeImageModel);
    imageModelFile = basename(installedPath);
    imageModelExists = true;
  }

  if (!enabled.llm && !enabled.stt && !enabled.image) {
    throw new Error(
      "No modalities enabled. Enable with --llm/--stt/--image true.",
    );
  }

  if (!enabled.llm && !parseFlag(args, "--llm")) {
    console.log("LLM route auto-disabled (no local LLM model file found).");
  }
  if (!enabled.stt && !parseFlag(args, "--stt")) {
    console.log("STT route auto-disabled (no local STT model file found).");
  }
  if (!enabled.image && !parseFlag(args, "--image")) {
    console.log("Image route auto-disabled (no local Image model file found).");
  }

  const llmBase = `http://${llmHost}:${llmPort}`;
  const sttBase = `http://${sttHost}:${sttPort}`;
  const imageBase = `http://${imageHost}:${imagePort}`;

  const llmService = enabled.llm
    ? new ManagedService(
        "llama-server",
        llmBase + "/health",
        ctx.logger,
        async () => {
          const activeModel = config.activeLlmModel;
          let modelFile = parseFlag(args, "--llm-model-file");
          if (!modelFile) {
            const spec = byId(activeModel);
            let expectedFile = spec?.filename;
            if (!expectedFile) {
              expectedFile = existsSync(
                join(config.llmModelsDir, `${activeModel}.bin`),
              )
                ? `${activeModel}.bin`
                : `${activeModel}.gguf`;
            }
            const modelPath = join(config.llmModelsDir, expectedFile);
            if (!existsSync(modelPath)) {
              ctx.logger.info(
                "llama-server",
                `Model file is missing for "${activeModel}". Automatically installing...`,
              );
              const installedPath = await installModel(config, activeModel);
              modelFile = basename(installedPath);
            } else {
              modelFile = expectedFile;
            }
          }

          let finalCtxSize = toInt(parseFlag(args, "--ctx-size"), 0);
          if (!finalCtxSize) {
            const spec = byId(activeModel);
            const recommendedCtx = spec
              ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
              : ctx.specs.gpuVramGb >= 32
                ? 32768
                : 8192;
            finalCtxSize = Math.min(recommendedCtx, config.ctxSize);
          }

          ctx.logger.info(
            "llama-server",
            `Spawning model "${activeModel}" (file: ${modelFile}, context: ${finalCtxSize} tokens)`,
          );
          return startLlamaServerProcess(
            config,
            modelFile,
            llmHost,
            llmPort,
            finalCtxSize,
          );
        },
      )
    : null;

  const sttService = enabled.stt
    ? new ManagedService(
        "whisper-server",
        sttBase + "/health",
        ctx.logger,
        () => startWhisperServerProcess(config, sttModelFile, sttHost, sttPort),
      )
    : null;

  const imageService = enabled.image
    ? new ManagedService("sd-server", imageBase + "/", ctx.logger, () => {
        const activeModel = config.activeImageModel;
        let modelFile = parseFlag(args, "--image-model-file");
        if (!modelFile) {
          const spec = byId(activeModel);
          let expectedFile = spec?.filename;
          if (!expectedFile) {
            expectedFile = `${activeModel}.safetensors`;
          }
          const modelPath = join(config.imageModelsDir, expectedFile);
          if (!existsSync(modelPath)) {
            throw new Error(`Image model file missing at ${modelPath}`);
          }
          modelFile = expectedFile;
        }

        ctx.logger.info(
          "sd-server",
          `Spawning image model "${activeModel}" (file: ${modelFile}) on port ${imagePort}`,
        );
        return startSdServerProcess(config, modelFile, imageHost, imagePort);
      })
    : null;

  const handleRequest = async (
    request: Request,
    pathname: string,
    method: string,
  ): Promise<Response> => {
    if (pathname === "/health") {
      return Response.json({
        status: "ok",
        enabled,
        llmUpstream: enabled.llm ? llmBase : null,
        sttUpstream: enabled.stt ? sttBase : null,
        imageUpstream: enabled.image ? imageBase : null,
        authRequired,
        authMode,
      });
    }

    if (authRequired) {
      const token = extractAuthToken(request, authMode);
      const isMasterKey =
        process.env.LOCALBASE_API_KEY &&
        token === process.env.LOCALBASE_API_KEY;
      if (!token || (!isMasterKey && !validateApiKey(config, token))) {
        return unauthorized(authMode);
      }
    }

    if (
      enabled.llm &&
      llmService &&
      (pathname === "/v1/chat/completions" ||
        pathname === "/v1/completions" ||
        pathname === "/v1/embeddings")
    ) {
      try {
        const bodyText = await request.clone().text();
        const bodyJson = JSON.parse(bodyText);
        if (bodyJson && typeof bodyJson.model === "string") {
          const requestedModel = bodyJson.model;
          const normalized = requestedModel.replace(
            /^(localbase|openai|ollama)\//,
            "",
          );
          const matchedModel = config.selectedLlmModels.find(
            (m) => m.toLowerCase() === normalized.toLowerCase(),
          );
          if (matchedModel && matchedModel !== config.activeLlmModel) {
            ctx.logger.info(
              "llama-server",
              `Switching active LLM from "${config.activeLlmModel}" to "${matchedModel}"`,
            );
            llmService.kill();
            config.activeLlmModel = matchedModel;
            saveConfig(config);

            const spec = byId(matchedModel);
            const recommendedCtx = spec
              ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb)
              : ctx.specs.gpuVramGb >= 32
                ? 32768
                : 8192;
            const newCtxSize = Math.min(recommendedCtx, config.ctxSize);

            syncContinueConfig(config, newCtxSize).catch((err) => {
              ctx.logger.warn(
                "sync",
                `Failed to sync Continue config: ${err.message}`,
              );
            });
          }
        }
      } catch (e) {
        // Ignore json body read issues
      }
    }

    if (
      pathname === "/v1/audio/transcriptions" ||
      pathname === "/v1/audio/translations"
    ) {
      if (!enabled.stt || !sttService) return notConfigured("STT");
      try {
        await sttService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("STT");
      }
      return proxyRequest(request, sttBase, sttPath);
    }

    if (pathname.startsWith("/stt")) {
      if (!enabled.stt || !sttService) return notConfigured("STT");
      try {
        await sttService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("STT");
      }
      const mapped = pathname.replace(/^\/stt/, "") || "/";
      return proxyRequest(request, sttBase, mapped);
    }

    if (pathname.startsWith("/llm")) {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      const mapped = pathname.replace(/^\/llm/, "") || "/";
      return proxyRequest(request, llmBase, mapped);
    }

    if (
      pathname.startsWith("/image") &&
      pathname !== "/v1/images/generations"
    ) {
      if (!enabled.image || !imageService) return notConfigured("Image");
      try {
        await imageService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("Image");
      }
      const mapped = pathname.replace(/^\/image/, "") || "/";
      return proxyRequest(request, imageBase, mapped);
    }

    if (pathname.startsWith("/tts")) {
      return Response.json(
        { error: "TTS service is not yet implemented. Stay tuned!" },
        { status: 501 },
      );
    }

    if (pathname.startsWith("/video")) {
      return Response.json(
        {
          error: "Video generation service is not yet implemented. Stay tuned!",
        },
        { status: 501 },
      );
    }

    if (pathname === "/v1/images/generations") {
      if (!enabled.image || !imageService) return notConfigured("Image");
      try {
        const bodyText = await request.clone().text();
        const bodyJson = JSON.parse(bodyText);
        if (bodyJson && typeof bodyJson.model === "string") {
          const requestedModel = bodyJson.model;
          const matchedModel = config.selectedImageModels.find(
            (m) => m.toLowerCase() === requestedModel.toLowerCase(),
          );
          if (matchedModel && matchedModel !== config.activeImageModel) {
            ctx.logger.info(
              "sd-server",
              `Switching active Image model from "${config.activeImageModel}" to "${matchedModel}"`,
            );
            imageService.kill();
            config.activeImageModel = matchedModel;
            saveConfig(config);
          }
        }
      } catch (e) {
        // Fall back if body parsing fails
      }
      try {
        await imageService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("Image");
      }
      return proxyRequest(request, imageBase);
    }

    if (pathname === "/v1/chat/completions") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      try {
        const bodyText = await request.clone().text();
        const bodyJson = JSON.parse(bodyText);
        if (bodyJson && Array.isArray(bodyJson.messages)) {
          let modified = false;
          for (const msg of bodyJson.messages) {
            // Map modern OpenAI 'developer' messages to 'system' because standard GGUF tokenizer
            // templates (e.g. Qwen, Llama) only recognize the 'system' role.
            if (msg.role === "developer") {
              msg.role = "system";
              modified = true;
            }
          }

          // Inject configured system prompt (or default fallback) if no system prompt is present
          const systemPrompt = config.systemPrompt || DEFAULT_SYSTEM_PROMPT;
          const hasSystem = bodyJson.messages.some(
            (msg: any) => msg.role === "system",
          );
          if (!hasSystem && systemPrompt) {
            bodyJson.messages.unshift({
              role: "system",
              content: systemPrompt,
            });
            modified = true;
          }

          if (modified) {
            const headers = new Headers(request.headers);
            // Delete Content-Length header to let fetch recalculate it for the modified JSON payload.
            headers.delete("content-length");
            const modifiedRequest = new Request(request.url, {
              method: request.method,
              headers,
              body: JSON.stringify(bodyJson),
            });
            return proxyRequest(modifiedRequest, llmBase);
          }
        }
      } catch (e) {
        // Fall back to default proxy if body parsing fails
      }
    }

    if (pathname === "/v1/models") {
      const modelsList = [
        ...new Set([config.activeLlmModel, ...config.selectedLlmModels]),
      ];
      const data = modelsList.map((modelId) => ({
        id: modelId,
        object: "model",
        created: 1670000000,
        owned_by: "local-base",
      }));
      return Response.json({
        object: "list",
        data,
      });
    }

    if (pathname === "/v1/slots") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      return proxyRequest(request, llmBase, "/slots");
    }

    if (pathname === "/v1/metrics") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      return proxyRequest(request, llmBase, "/metrics");
    }

    if (pathname === "/v1/props") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      return proxyRequest(request, llmBase, "/props");
    }

    if (pathname === "/v1/system_info") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try {
        await llmService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("LLM");
      }
      return proxyRequest(request, llmBase, "/system_info");
    }

    if (!enabled.llm || !llmService) return notConfigured("LLM");
    try {
      await llmService.ensureRunning();
    } catch (err) {
      return serviceUnavailable("LLM");
    }
    return proxyRequest(request, llmBase);
  };

  const server = Bun.serve({
    hostname: wrapperHost,
    port: wrapperPort,
    fetch: async (request) => {
      const start = performance.now();
      const ip = server.requestIP(request)?.address ?? "127.0.0.1";
      const { pathname } = new URL(request.url);
      const method = request.method;

      if (method === "OPTIONS") {
        return new Response(null, {
          status: 204,
          headers: {
            "Access-Control-Allow-Origin": "*",
            "Access-Control-Allow-Methods": "GET, POST, PUT, DELETE, OPTIONS",
            "Access-Control-Allow-Headers":
              "Content-Type, Authorization, x-api-key",
            "Access-Control-Max-Age": "86400",
          },
        });
      }

      let response: Response;
      try {
        response = await handleRequest(request, pathname, method);
      } catch (err) {
        ctx.logger.error(
          "HTTP",
          `Error handling request ${method} ${pathname}`,
          err as Error,
        );
        response = Response.json(
          { error: "Internal Server Error" },
          { status: 500 },
        );
      }

      const headers = new Headers(response.headers);
      headers.set("Access-Control-Allow-Origin", "*");
      headers.set(
        "Access-Control-Allow-Methods",
        "GET, POST, PUT, DELETE, OPTIONS",
      );
      headers.set(
        "Access-Control-Allow-Headers",
        "Content-Type, Authorization, x-api-key",
      );

      const corsResponse = new Response(response.body, {
        status: response.status,
        statusText: response.statusText,
        headers,
      });

      const durationMs = performance.now() - start;
      ctx.logger.request(ip, method, pathname, corsResponse.status, durationMs);
      return corsResponse;
    },
  });

  printUnifiedNextSteps(
    wrapperHost,
    wrapperPort,
    llmPort,
    sttPort,
    imagePort,
    authRequired,
    authMode,
    enabled,
  );

  const shutdown = () => {
    ctx.logger.info("Manager", "Shutting down servers and subprocesses...");
    server.stop(true);
    llmService?.kill();
    sttService?.kill();
    imageService?.kill();
  };

  process.on("SIGINT", () => {
    shutdown();
    process.exit(0);
  });
  process.on("SIGTERM", () => {
    shutdown();
    process.exit(0);
  });

  // Keep alive forever
  await new Promise(() => {});
  return 0;
}
