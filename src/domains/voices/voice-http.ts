import type { ILogger } from "../observability/logging";
import {
  CUSTOM_VOICE_LIMIT,
  customSpeechVoiceIdSchema,
  VOICE_CONSENT_STATEMENT,
  VOICE_MAX_SECONDS,
  VOICE_MAX_UPLOAD_BYTES,
  VOICE_MIN_SECONDS,
  VoiceError,
  customVoiceListSchema,
  customVoiceRenameSchema,
  customVoiceSchema,
  type CustomVoice,
} from "./voice-contract";
import type { CustomVoiceRecord, CustomVoiceStore } from "./voice-store";

export const VOICES_PATH = "/_localbase/voices";
const voicePathPrefix = `${VOICES_PATH}/`;
/** Multipart overhead allowed beyond the audio itself (name, consent, boundaries). */
const MULTIPART_OVERHEAD_BYTES = 16 * 1024;

export type VoiceRoute =
  | Readonly<{ kind: "collection" }>
  | Readonly<{ kind: "voice"; id: string }>
  | Readonly<{ kind: "audio"; id: string }>;

/** Matches `/_localbase/voices`, `/{id}` and `/{id}/audio`; ids are never decoded. */
export function voiceRouteFromPath(pathname: string): VoiceRoute | undefined {
  if (pathname === VOICES_PATH) return { kind: "collection" };
  if (!pathname.startsWith(voicePathPrefix)) return undefined;
  const [id, suffix, ...rest] = pathname
    .slice(voicePathPrefix.length)
    .split("/");
  if (!id || rest.length > 0) return undefined;
  if (!customSpeechVoiceIdSchema.safeParse(id).success) return undefined;
  if (suffix === undefined) return { kind: "voice", id };
  return suffix === "audio" ? { kind: "audio", id } : undefined;
}

export type VoiceHttpDependencies = Readonly<{
  store: CustomVoiceStore;
  /** Owner id of an authenticated principal holding `models:manage`. */
  manager: string | undefined;
  /** Whether the principal holds `models:read`. */
  canRead: boolean;
  readBody: (request: Request, limit: number) => Promise<Uint8Array>;
  isPayloadTooLarge: (error: unknown) => boolean;
  logger: ILogger;
}>;

const noStore = { "cache-control": "no-store" } as const;

function errorResponse(
  status: number,
  message: string,
  code: string,
  param: string | null = null,
  headers: HeadersInit = noStore,
): Response {
  return Response.json(
    { error: { message, type: "invalid_request_error", param, code } },
    { status, headers },
  );
}

function voiceErrorResponse(error: VoiceError): Response {
  switch (error.code) {
    case "voice_not_found":
      return errorResponse(404, error.message, error.code);
    case "voice_name_taken":
    case "voice_limit_reached":
      return errorResponse(409, error.message, error.code, "name");
    case "audio_too_large":
      return errorResponse(413, error.message, error.code, "audio");
    case "consent_required":
      return errorResponse(400, error.message, error.code, "consent");
    case "invalid_voice_name":
      return errorResponse(400, error.message, error.code, "name");
    default:
      return errorResponse(400, error.message, error.code, "audio");
  }
}

function publicVoice(record: CustomVoiceRecord, canManage: boolean) {
  const { createdBy, ...voice } = record;
  return customVoiceSchema.parse(
    canManage ? { ...voice, createdBy } : voice,
  ) satisfies CustomVoice;
}

function logChange(
  logger: ILogger,
  action: "created" | "renamed" | "deleted",
  record: CustomVoiceRecord,
): void {
  // Ids only: display names can be personal names, and audio never leaves the store.
  logger.event({
    severity: "info",
    eventName: `voices.${action}`,
    category: "gateway",
    component: "voices",
    runtime: "tts",
    message: `Custom voice ${action}.`,
    attributes: {
      voice_id: record.id,
      ...(action === "created" ? { duration_ms: record.durationMs } : {}),
    },
  });
}

async function parseCreate(
  request: Request,
  dependencies: VoiceHttpDependencies,
): Promise<
  | { kind: "ok"; name: string; consent: boolean; audio: Uint8Array }
  | { kind: "error"; response: Response }
> {
  const contentType = request.headers.get("content-type") ?? "";
  if (!contentType.toLowerCase().startsWith("multipart/form-data;")) {
    return {
      kind: "error",
      response: errorResponse(
        415,
        "Send multipart/form-data with name, consent and audio fields.",
        "unsupported_media_type",
      ),
    };
  }
  let form: FormData;
  try {
    const bytes = await dependencies.readBody(
      request,
      VOICE_MAX_UPLOAD_BYTES + MULTIPART_OVERHEAD_BYTES,
    );
    const body = new ArrayBuffer(bytes.byteLength);
    new Uint8Array(body).set(bytes);
    form = await new Request(request.url, {
      method: "POST",
      headers: { "content-type": contentType },
      body,
    }).formData();
  } catch (error) {
    if (dependencies.isPayloadTooLarge(error)) {
      return {
        kind: "error",
        response: errorResponse(
          413,
          `Audio exceeds the ${VOICE_MAX_UPLOAD_BYTES / (1024 * 1024)} MiB limit.`,
          "audio_too_large",
          "audio",
          { ...noStore, connection: "close" },
        ),
      };
    }
    throw error;
  }
  const name = form.get("name");
  const consent = form.get("consent");
  const audio = form.get("audio");
  if (typeof name !== "string") {
    return {
      kind: "error",
      response: errorResponse(
        400,
        "Voice name is required.",
        "invalid_voice_name",
        "name",
      ),
    };
  }
  if (!(audio instanceof File)) {
    return {
      kind: "error",
      response: errorResponse(
        400,
        "Attach the reference recording as the audio field.",
        "unsupported_audio_format",
        "audio",
      ),
    };
  }
  // The upload's filename and declared type are ignored; only its bytes count.
  return {
    kind: "ok",
    name,
    consent: consent === "true",
    audio: new Uint8Array(await audio.arrayBuffer()),
  };
}

/** Serves `/_localbase/voices`. Callers resolve authentication first. */
export async function handleVoiceRequest(
  request: Request,
  route: VoiceRoute,
  dependencies: VoiceHttpDependencies,
): Promise<Response> {
  const { store, manager } = dependencies;
  const canManage = manager !== undefined;
  const forbidden = () =>
    errorResponse(
      403,
      "Forbidden: The credential lacks the required permission.",
      "insufficient_permissions",
    );
  const methodNotAllowed = (allow: string) =>
    errorResponse(405, "Method not allowed.", "method_not_allowed", null, {
      ...noStore,
      allow,
    });
  try {
    if (route.kind === "collection") {
      if (request.method === "GET") {
        if (!dependencies.canRead) return forbidden();
        return Response.json(
          customVoiceListSchema.parse({
            object: "list",
            data: store.list().map((record) => publicVoice(record, canManage)),
            canManage,
            limits: {
              minSeconds: VOICE_MIN_SECONDS,
              maxSeconds: VOICE_MAX_SECONDS,
              maxUploadBytes: VOICE_MAX_UPLOAD_BYTES,
              maxVoices: CUSTOM_VOICE_LIMIT,
            },
            consentStatement: VOICE_CONSENT_STATEMENT,
          }),
          { headers: noStore },
        );
      }
      if (request.method !== "POST") return methodNotAllowed("GET, POST");
      if (!canManage) return forbidden();
      const parsed = await parseCreate(request, dependencies);
      if (parsed.kind === "error") return parsed.response;
      const record = await store.create({
        name: parsed.name,
        consent: parsed.consent,
        audio: parsed.audio,
        createdBy: manager,
      });
      logChange(dependencies.logger, "created", record);
      return Response.json(publicVoice(record, true), {
        status: 201,
        headers: noStore,
      });
    }

    if (route.kind === "audio") {
      if (request.method !== "GET") return methodNotAllowed("GET");
      // The reference is a recording of a person; only managers replay it.
      if (!canManage) return forbidden();
      const path = await store.resolve(route.id);
      if (!path) throw new VoiceError("voice_not_found", "Voice not found.");
      const bytes = new Uint8Array(await Bun.file(path).arrayBuffer());
      return new Response(bytes, {
        headers: {
          ...noStore,
          "content-type": "audio/wav",
          "content-length": String(bytes.byteLength),
          "x-content-type-options": "nosniff",
        },
      });
    }

    if (request.method === "GET") {
      if (!dependencies.canRead) return forbidden();
      const record = store.get(route.id);
      if (!record) throw new VoiceError("voice_not_found", "Voice not found.");
      return Response.json(publicVoice(record, canManage), {
        headers: noStore,
      });
    }
    if (request.method === "PATCH") {
      if (!canManage) return forbidden();
      let input: unknown;
      try {
        input = JSON.parse(
          new TextDecoder().decode(await dependencies.readBody(request, 1024)),
        );
      } catch (error) {
        if (dependencies.isPayloadTooLarge(error)) {
          return errorResponse(
            413,
            "Voice update body exceeds the 1024 byte limit.",
            "validation_failed",
            null,
            { ...noStore, connection: "close" },
          );
        }
        return errorResponse(
          400,
          "Send a JSON body with a name.",
          "validation_failed",
        );
      }
      const parsed = customVoiceRenameSchema.safeParse(input);
      if (!parsed.success) {
        return errorResponse(
          400,
          parsed.error.issues[0]?.message ?? "Invalid voice name.",
          "invalid_voice_name",
          "name",
        );
      }
      const record = store.rename(route.id, parsed.data.name);
      logChange(dependencies.logger, "renamed", record);
      return Response.json(publicVoice(record, true), { headers: noStore });
    }
    if (request.method === "DELETE") {
      if (!canManage) return forbidden();
      const record = await store.remove(route.id);
      logChange(dependencies.logger, "deleted", record);
      return Response.json(
        { id: record.id, object: "localbase.voice", deleted: true },
        { headers: noStore },
      );
    }
    return methodNotAllowed("GET, PATCH, DELETE");
  } catch (error) {
    if (error instanceof VoiceError) return voiceErrorResponse(error);
    throw error;
  }
}
