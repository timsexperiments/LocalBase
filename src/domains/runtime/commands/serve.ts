import { existsSync } from "node:fs";
import { join } from "node:path";
import { type LocalBaseConfig, startLlamaServerProcess, startWhisperServerProcess, validateApiKey } from "../../../manager";
import { parseBool, parseFlag, toInt } from "../../../utils/args";

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

export async function runServe(args: string[], config: LocalBaseConfig): Promise<number> {
  const wrapperHost = parseFlag(args, "--host") ?? "0.0.0.0";
  const wrapperPort = toInt(parseFlag(args, "--port"), 8787);

  const llmHost = parseFlag(args, "--llm-host") ?? "127.0.0.1";
  const llmPort = toInt(parseFlag(args, "--llm-port"), config.port);
  const sttHost = parseFlag(args, "--stt-host") ?? "127.0.0.1";
  const sttPort = toInt(parseFlag(args, "--stt-port"), config.sttPort);
  const ctxSize = toInt(parseFlag(args, "--ctx-size"), config.ctxSize);
  const sttPath = parseFlag(args, "--stt-path") ?? "/inference";
  const authRequired = parseFlag(args, "--auth") !== "false";
  const authMode = parseAuthMode(parseFlag(args, "--auth-mode"));

  const ttsUpstream = parseFlag(args, "--tts-upstream");
  const imageUpstream = parseFlag(args, "--image-upstream");
  const videoUpstream = parseFlag(args, "--video-upstream");

  const llmModelFile = parseFlag(args, "--llm-model-file") ?? `${config.activeLlmModel}.gguf`;
  const sttModelFile = parseFlag(args, "--stt-model-file") ?? `${config.activeSttModel}.gguf`;

  const llmModelExists = existsSync(join(config.llmModelsDir, llmModelFile));
  const sttModelExists = existsSync(join(config.sttModelsDir, sttModelFile));

  const enabled: ModalityState = {
    llm: parseBool(parseFlag(args, "--llm"), llmModelExists),
    stt: parseBool(parseFlag(args, "--stt"), sttModelExists),
    tts: parseBool(parseFlag(args, "--tts"), Boolean(ttsUpstream)),
    image: parseBool(parseFlag(args, "--image"), Boolean(imageUpstream)),
    video: parseBool(parseFlag(args, "--video"), Boolean(videoUpstream))
  };

  if (enabled.llm && !llmModelExists) {
    throw new Error(`LLM enabled but model file is missing: ${join(config.llmModelsDir, llmModelFile)}. Install a model or pass --llm false.`);
  }
  if (enabled.stt && !sttModelExists) {
    throw new Error(`STT enabled but model file is missing: ${join(config.sttModelsDir, sttModelFile)}. Install a model or pass --stt false.`);
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

  const llmProc = enabled.llm ? startLlamaServerProcess(config, llmModelFile, llmHost, llmPort, ctxSize) : null;
  const sttProc = enabled.stt ? startWhisperServerProcess(config, sttModelFile, sttHost, sttPort) : null;

  const llmBase = `http://${llmHost}:${llmPort}`;
  const sttBase = `http://${sttHost}:${sttPort}`;

  const server = Bun.serve({
    hostname: wrapperHost,
    port: wrapperPort,
    fetch: async (request) => {
      const { pathname } = new URL(request.url);
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
        if (!enabled.stt) return notConfigured("STT");
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
        if (!enabled.stt) return notConfigured("STT");
        const mapped = pathname.replace(/^\/stt/, "") || "/";
        return proxyRequest(request, sttBase, mapped);
      }

      if (!enabled.llm) return notConfigured("LLM");
      return proxyRequest(request, llmBase);
    }
  });

  printUnifiedNextSteps(wrapperHost, wrapperPort, llmPort, sttPort, authRequired, authMode, enabled, ttsUpstream ?? undefined, imageUpstream ?? undefined, videoUpstream ?? undefined);

  const shutdown = () => {
    server.stop(true);
    llmProc?.kill();
    sttProc?.kill();
  };

  process.on("SIGINT", () => {
    shutdown();
    process.exit(0);
  });
  process.on("SIGTERM", () => {
    shutdown();
    process.exit(0);
  });

  const exitWaiters: Promise<"llm" | "stt">[] = [];
  if (llmProc) exitWaiters.push(llmProc.exited.then(() => "llm" as const));
  if (sttProc) exitWaiters.push(sttProc.exited.then(() => "stt" as const));

  if (exitWaiters.length === 0) {
    await new Promise(() => {});
  }

  const winner = await Promise.race(exitWaiters);

  shutdown();
  console.error(`Unified wrapper stopped because ${winner} runtime exited.`);
  return 1;
}
