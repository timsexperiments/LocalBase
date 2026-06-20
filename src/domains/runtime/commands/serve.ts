import { existsSync } from "node:fs";
import { join, basename } from "node:path";
import { type LocalBaseConfig, startLlamaServerProcess, startWhisperServerProcess, validateApiKey, installModel } from "../../../manager";
import { byId, evaluateModelFit, calculateMaxSafeContextSize } from "../../../catalog";
import type { AppContext } from "../../../context";
import { parseBool, parseFlag, toInt } from "../../../utils/args";
import { syncOpenCodeConfig } from "../../config/commands/configure";
import { Logger } from "../../../utils/logger";


type AuthMode = "bearer" | "x-api-key" | "either";

type ModalityState = {
  llm: boolean;
  stt: boolean;
  tts: boolean;
  image: boolean;
  video: boolean;
};

function parseAuthMode(raw: string | undefined): AuthMode {
  if (!raw) return "either";
  if (raw === "bearer" || raw === "x-api-key" || raw === "either") return raw;
  throw new Error(`Invalid --auth-mode value: ${raw}. Expected bearer|x-api-key|either`);
}

function printUnifiedNextSteps(
  host: string,
  port: number,
  llmPort: number,
  sttPort: number,
  authRequired: boolean,
  authMode: AuthMode,
  enabled: ModalityState,
  ttsUpstream?: string,
  imageUpstream?: string,
  videoUpstream?: string
): void {
  console.log("\nUnified API wrapper started.");
  console.log(`Wrapper base URL: http://${host}:${port}`);
  if (enabled.llm) console.log(`OpenAI-compatible LLM endpoint: http://${host}:${port}/v1`);
  if (enabled.stt) console.log(`OpenAI-compatible STT endpoint: http://${host}:${port}/v1/audio/transcriptions`);
  if (authRequired) {
    console.log(`Authentication: enabled (mode=${authMode}).`);
    console.log("Supported credentials: Authorization: Bearer <key>, x-api-key: <key> (mode-dependent).");
  } else {
    console.log("Authentication: disabled via --auth false.");
  }
  console.log(`Enabled modalities: ${Object.entries(enabled).filter(([, on]) => on).map(([k]) => k).join(", ") || "none"}`);
  if (enabled.llm) console.log(`Upstream llama-server: http://127.0.0.1:${llmPort}`);
  if (enabled.stt) console.log(`Upstream whisper-server: http://127.0.0.1:${sttPort}`);
  if (enabled.tts && ttsUpstream) console.log(`Upstream TTS: ${ttsUpstream}`);
  if (enabled.image && imageUpstream) console.log(`Upstream Image: ${imageUpstream}`);
  if (enabled.video && videoUpstream) console.log(`Upstream Video: ${videoUpstream}`);

  if (enabled.llm) {
    console.log("\nExample chat request (Bearer):");
    console.log(
      `curl http://${host}:${port}/v1/chat/completions -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"<your-model>","messages":[{"role":"user","content":"hello"}]}'`
    );
  }
  if (enabled.stt) {
    console.log("\nExample STT request (x-api-key):");
    console.log(`curl -X POST http://${host}:${port}/v1/audio/transcriptions -H 'x-api-key: <API_KEY>' -F file=@audio.wav -F model=whisper`);
  }
  if (enabled.tts) console.log(`\nExample TTS request: curl -X POST http://${host}:${port}/v1/audio/speech -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"model":"tts","input":"hello"}'`);
  if (enabled.image) console.log(`\nExample image request: curl -X POST http://${host}:${port}/v1/images/generations -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' -d '{"prompt":"a cat"}'`);
}

function extractBearerToken(request: Request): string | null {
  const auth = request.headers.get("authorization") ?? request.headers.get("Authorization");
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
  return extractBearerToken(request) ?? extractApiKeyHeader(request)?.trim() ?? null;
}

function unauthorized(mode: AuthMode): Response {
  const hint = mode === "x-api-key" ? "x-api-key" : "Bearer";
  return Response.json(
    { error: "Unauthorized", expected: mode === "either" ? "Bearer or x-api-key" : hint },
    { status: 401, headers: { "www-authenticate": "Bearer" } }
  );
}

function notConfigured(feature: string): Response {
  return Response.json(
    {
      error: `${feature} route is disabled`,
      hint: `Set --${feature.toLowerCase()} true and configure upstream/model to enable this route`
    },
    { status: 501 }
  );
}

async function proxyRequest(request: Request, targetBase: string, pathOverride?: string): Promise<Response> {
  const incoming = new URL(request.url);
  const path = pathOverride ?? incoming.pathname;
  const target = `${targetBase}${path}${incoming.search}`;
  const upstream = await fetch(target, {
    method: request.method,
    headers: request.headers,
    body: request.body
  });

  return new Response(upstream.body, {
    status: upstream.status,
    headers: upstream.headers
  });
}

class ManagedService {
  private proc: Bun.Subprocess | null = null;
  private name: string;
  private startFn: () => Promise<Bun.Subprocess>;
  private healthUrl: string;
  private crashTimes: number[] = [];
  private isRestarting = false;
  private restartPromise: Promise<void> | null = null;

  constructor(name: string, healthUrl: string, startFn: () => Promise<Bun.Subprocess>) {
    this.name = name;
    this.healthUrl = healthUrl;
    this.startFn = startFn;
  }

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

  private async start(): Promise<void> {
    this.isRestarting = true;
    this.restartPromise = (async () => {
      const now = Date.now();
      this.crashTimes = this.crashTimes.filter(t => now - t < 300000); // 5 min window
      const crashCount = this.crashTimes.length;

      if (crashCount >= 5) {
        Logger.error(this.name, `Service has crashed ${crashCount} times in 5 minutes. Stopping manager.`);
        process.exit(1);
      }

      if (crashCount > 0) {
        const backoffMs = Math.min(1000 * Math.pow(2, crashCount - 1), 16000);
        Logger.warn(this.name, `Crashed previously. Backing off for ${backoffMs}ms before restarting...`);
        await new Promise(resolve => setTimeout(resolve, backoffMs));
      }

      Logger.info(this.name, "Starting subprocess...");
      try {
        this.proc = await this.startFn();
        if (this.proc.stdout && typeof this.proc.stdout !== "number") Logger.pipeStream(this.proc.stdout, this.name);
        if (this.proc.stderr && typeof this.proc.stderr !== "number") Logger.pipeStream(this.proc.stderr, this.name);

        this.proc.exited.then(() => {
          this.handleCrash();
        });

        const ok = await this.waitHealthy();
        if (!ok) {
          throw new Error("Health check failed to pass within timeout");
        }
        Logger.info(this.name, "Subprocess is healthy and ready.");
      } catch (err) {
        Logger.error(this.name, "Failed to start service", err as Error);
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

  private async waitHealthy(): Promise<boolean> {
    const timeout = 30000; // 30s
    const interval = 200; // 200ms
    const start = Date.now();

    while (Date.now() - start < timeout) {
      if (this.proc?.exitCode !== null) {
        Logger.error(this.name, `Subprocess exited during startup with code ${this.proc?.exitCode}`);
        return false;
      }
      try {
        const res = await fetch(this.healthUrl);
        if (res.ok) return true;
      } catch (e) {
        // Expected during startup
      }
      await new Promise(resolve => setTimeout(resolve, interval));
    }
    return false;
  }

  kill(): void {
    if (this.proc) {
      this.proc.kill();
      this.proc = null;
    }
  }

  handleCrash(): void {
    if (this.proc && this.proc.exitCode !== null && !this.isRestarting) {
      Logger.warn(this.name, `Subprocess exited unexpectedly with code ${this.proc.exitCode}. Triggering self-healing...`);
      this.crashTimes.push(Date.now());
      this.proc = null;
      this.start().catch(() => {});
    }
  }
}

function serviceUnavailable(serviceName: string): Response {
  return new Response(
    JSON.stringify({ error: `${serviceName} service is currently restarting or unavailable. Please try again shortly.` }),
    {
      status: 503,
      headers: {
        "Content-Type": "application/json",
        "Retry-After": "5"
      }
    }
  );
}

export async function runServe(args: string[], ctx: AppContext): Promise<number> {
  const config = ctx.config;
  const wrapperHost = parseFlag(args, "--host") ?? "0.0.0.0";
  const wrapperPort = toInt(parseFlag(args, "--port"), 8787);

  const llmHost = parseFlag(args, "--llm-host") ?? "127.0.0.1";
  const llmPort = toInt(parseFlag(args, "--llm-port"), config.port);
  const sttHost = parseFlag(args, "--stt-host") ?? "127.0.0.1";
  const sttPort = toInt(parseFlag(args, "--stt-port"), config.sttPort);
  let ctxSize = toInt(parseFlag(args, "--ctx-size"), 0);
  if (!ctxSize) {
    const spec = byId(config.activeLlmModel);
    const recommendedCtx = spec ? calculateMaxSafeContextSize(spec, ctx.specs.gpuVramGb) : (ctx.specs.gpuVramGb >= 32 ? 32768 : 8192);
    ctxSize = Math.min(recommendedCtx, config.ctxSize);
    console.log(`\n💡 Dynamic Context Size: Initialized to ${ctxSize} tokens.`);
    console.log(`   (Calculated best for "${config.activeLlmModel}" on ${ctx.specs.gpuVramGb} GB hardware: ${recommendedCtx} tokens, limited by max context setting: ${config.ctxSize} tokens)`);
  } else {
    console.log(`\n💡 Context Size: Using explicit override --ctx-size ${ctxSize} tokens.`);
  }

  // Automatically synchronize active model and calculated context size with OpenCode configuration
  await syncOpenCodeConfig(config, ctxSize);
  const sttPath = parseFlag(args, "--stt-path") ?? "/inference";
  const authRequired = parseFlag(args, "--auth") !== "false";
  const authMode = parseAuthMode(parseFlag(args, "--auth-mode"));

  const ttsUpstream = parseFlag(args, "--tts-upstream");
  const imageUpstream = parseFlag(args, "--image-upstream");
  const videoUpstream = parseFlag(args, "--video-upstream");

  let llmModelFile = parseFlag(args, "--llm-model-file");
  if (!llmModelFile) {
    const spec = byId(config.activeLlmModel);
    if (spec?.filename && existsSync(join(config.llmModelsDir, spec.filename))) {
      llmModelFile = spec.filename;
    } else if (existsSync(join(config.llmModelsDir, `${config.activeLlmModel}.bin`))) {
      llmModelFile = `${config.activeLlmModel}.bin`;
    } else {
      llmModelFile = `${config.activeLlmModel}.gguf`;
    }
  }

  let sttModelFile = parseFlag(args, "--stt-model-file");
  if (!sttModelFile) {
    const spec = byId(config.activeSttModel);
    if (spec?.filename && existsSync(join(config.sttModelsDir, spec.filename))) {
      sttModelFile = spec.filename;
    } else if (existsSync(join(config.sttModelsDir, `${config.activeSttModel}.bin`))) {
      sttModelFile = `${config.activeSttModel}.bin`;
    } else {
      sttModelFile = `${config.activeSttModel}.gguf`;
    }
  }

  let llmModelExists = existsSync(join(config.llmModelsDir, llmModelFile));
  let sttModelExists = existsSync(join(config.sttModelsDir, sttModelFile));

  const enabled: ModalityState = {
    llm: parseBool(parseFlag(args, "--llm"), true),
    stt: parseBool(parseFlag(args, "--stt"), true),
    tts: parseBool(parseFlag(args, "--tts"), Boolean(ttsUpstream)),
    image: parseBool(parseFlag(args, "--image"), Boolean(imageUpstream)),
    video: parseBool(parseFlag(args, "--video"), Boolean(videoUpstream))
  };

  // Perform memory fit evaluation BEFORE downloading
  const specs = ctx.specs;
  const getModelIdFromFile = (filename: string): string => {
    return filename.replace(/\.(gguf|bin|onnx|safetensors|pth)$/i, "");
  };

  const bypassCheck = args.includes("--bypass-memory-check") || args.includes("--force");

  if (enabled.llm) {
    const llmModelId = getModelIdFromFile(llmModelFile);
    const llmSpec = byId(llmModelId);
    if (llmSpec) {
      const fit = evaluateModelFit(llmSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(`\n❌ ERROR: Insufficient VRAM/Unified Memory to run LLM "${llmSpec.modelId}".`);
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.error(`   Running this model will likely crash the system or cause severe slowdowns.`);
        if (!bypassCheck) {
          console.error(`   To force launch this model anyway, use --bypass-memory-check`);
          return 1;
        } else {
          console.warn(`   Bypassing memory validation check and proceeding...`);
        }
      } else if (fit.status === "tight") {
        console.warn(`\n⚠️ WARNING: Tight memory fit for LLM "${llmSpec.modelId}".`);
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
        console.warn(`   Leaves only ${fit.headroomGb.toFixed(1)} GB headroom. Large context windows may cause slowdowns.`);
      } else {
        console.log(`\n✅ Memory check passed: LLM "${llmSpec.modelId}" fits comfortably in ${specs.gpuVramGb} GB.`);
      }

      const maxSafeCtx = calculateMaxSafeContextSize(llmSpec, specs.gpuVramGb);
      if (ctxSize < maxSafeCtx) {
        console.log(`\n💡 Tip: Your system supports a larger context size of up to ${maxSafeCtx} tokens for "${llmSpec.modelId}".`);
        console.log(`   You can configure this by running 'local-base configure' or starting with '--ctx-size ${maxSafeCtx}'.`);
      }
    }
  }

  if (enabled.stt) {
    const sttModelId = getModelIdFromFile(sttModelFile);
    const sttSpec = byId(sttModelId);
    if (sttSpec) {
      const fit = evaluateModelFit(sttSpec, specs.gpuVramGb);
      if (fit.status === "insufficient") {
        console.error(`\n❌ ERROR: Insufficient VRAM/Unified Memory to run STT "${sttSpec.modelId}".`);
        console.error(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.error(`   Detected host memory:      ${fit.systemVramGb} GB`);
        if (!bypassCheck) {
          console.error(`   To force launch this model anyway, use --bypass-memory-check`);
          return 1;
        } else {
          console.warn(`   Bypassing memory validation check and proceeding...`);
        }
      } else if (fit.status === "tight") {
        console.warn(`\n⚠️ WARNING: Tight memory fit for STT "${sttSpec.modelId}".`);
        console.warn(`   Model minimum requirement: ${fit.minVramGb} GB`);
        console.warn(`   Detected host memory:      ${fit.systemVramGb} GB`);
      }
    }
  }

  // Automatically download models if they pass memory checks and are missing
  if (enabled.llm && !llmModelExists) {
    console.log(`LLM model file is missing. Automatically installing "${config.activeLlmModel}"...`);
    const installedPath = installModel(config, config.activeLlmModel);
    llmModelFile = basename(installedPath);
    llmModelExists = true;
  }

  if (enabled.stt && !sttModelExists) {
    console.log(`STT model file is missing. Automatically installing "${config.activeSttModel}"...`);
    const installedPath = installModel(config, config.activeSttModel);
    sttModelFile = basename(installedPath);
    sttModelExists = true;
  }

  if (enabled.tts && !ttsUpstream) throw new Error("TTS enabled but --tts-upstream is not set.");
  if (enabled.image && !imageUpstream) throw new Error("Image enabled but --image-upstream is not set.");
  if (enabled.video && !videoUpstream) throw new Error("Video enabled but --video-upstream is not set.");

  if (!enabled.llm && !enabled.stt && !enabled.tts && !enabled.image && !enabled.video) {
    throw new Error("No modalities enabled. Install/configure models or upstreams, or enable with --llm/--stt/--tts/--image/--video true.");
  }

  if (!enabled.llm && !parseFlag(args, "--llm")) {
    console.log("LLM route auto-disabled (no local LLM model file found).");
  }
  if (!enabled.stt && !parseFlag(args, "--stt")) {
    console.log("STT route auto-disabled (no local STT model file found).");
  }


  const llmBase = `http://${llmHost}:${llmPort}`;
  const sttBase = `http://${sttHost}:${sttPort}`;

  const llmService = enabled.llm
    ? new ManagedService("llama-server", llmBase + "/health", () =>
        startLlamaServerProcess(config, llmModelFile, llmHost, llmPort, ctxSize)
      )
    : null;

  const sttService = enabled.stt
    ? new ManagedService("whisper-server", sttBase + "/health", () =>
        startWhisperServerProcess(config, sttModelFile, sttHost, sttPort)
      )
    : null;

  const handleRequest = async (request: Request, pathname: string, method: string): Promise<Response> => {
    if (pathname === "/health") {
      return Response.json({ status: "ok", enabled, llmUpstream: enabled.llm ? llmBase : null, sttUpstream: enabled.stt ? sttBase : null, ttsUpstream: enabled.tts ? ttsUpstream : null, imageUpstream: enabled.image ? imageUpstream : null, videoUpstream: enabled.video ? videoUpstream : null, authRequired, authMode });
    }

    if (authRequired) {
      const token = extractAuthToken(request, authMode);
      if (!token || !validateApiKey(config, token)) {
        return unauthorized(authMode);
      }
    }

    if (pathname === "/v1/audio/transcriptions" || pathname === "/v1/audio/translations") {
      if (!enabled.stt || !sttService) return notConfigured("STT");
      try {
        await sttService.ensureRunning();
      } catch (err) {
        return serviceUnavailable("STT");
      }
      return proxyRequest(request, sttBase, sttPath);
    }

    if (pathname === "/v1/audio/speech") {
      if (!enabled.tts || !ttsUpstream) return notConfigured("TTS");
      return proxyRequest(request, ttsUpstream);
    }

    if (pathname.startsWith("/v1/images")) {
      if (!enabled.image || !imageUpstream) return notConfigured("Image");
      return proxyRequest(request, imageUpstream);
    }

    if (pathname.startsWith("/v1/video")) {
      if (!enabled.video || !videoUpstream) return notConfigured("Video");
      return proxyRequest(request, videoUpstream);
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
            if (msg.role === "developer") {
              msg.role = "system";
              modified = true;
            }
          }
          if (modified) {
            const headers = new Headers(request.headers);
            headers.delete("content-length");
            const modifiedRequest = new Request(request.url, {
              method: request.method,
              headers,
              body: JSON.stringify(bodyJson)
            });
            return proxyRequest(modifiedRequest, llmBase);
          }
        }
      } catch (e) {
        // Fall back to default proxy if parsing fails
      }
    }

    if (pathname === "/v1/slots") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try { await llmService.ensureRunning(); } catch (err) { return serviceUnavailable("LLM"); }
      return proxyRequest(request, llmBase, "/slots");
    }

    if (pathname === "/v1/metrics") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try { await llmService.ensureRunning(); } catch (err) { return serviceUnavailable("LLM"); }
      return proxyRequest(request, llmBase, "/metrics");
    }

    if (pathname === "/v1/props") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try { await llmService.ensureRunning(); } catch (err) { return serviceUnavailable("LLM"); }
      return proxyRequest(request, llmBase, "/props");
    }

    if (pathname === "/v1/system_info") {
      if (!enabled.llm || !llmService) return notConfigured("LLM");
      try { await llmService.ensureRunning(); } catch (err) { return serviceUnavailable("LLM"); }
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

      let response: Response;
      try {
        response = await handleRequest(request, pathname, method);
      } catch (err) {
        Logger.error("HTTP", `Error handling request ${method} ${pathname}`, err as Error);
        response = Response.json({ error: "Internal Server Error" }, { status: 500 });
      }

      const durationMs = performance.now() - start;
      Logger.request(ip, method, pathname, response.status, durationMs);
      return response;
    }
  });

  printUnifiedNextSteps(wrapperHost, wrapperPort, llmPort, sttPort, authRequired, authMode, enabled, ttsUpstream ?? undefined, imageUpstream ?? undefined, videoUpstream ?? undefined);

  const shutdown = () => {
    Logger.info("Manager", "Shutting down servers and subprocesses...");
    server.stop(true);
    llmService?.kill();
    sttService?.kill();
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
