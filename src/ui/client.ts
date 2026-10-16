import { z } from "zod";
import { deltaReasoning, thinkSplitter } from "./reasoning";
import type { Capability } from "../catalog/capabilities";
import {
  permissionsSchema,
  type Permission,
} from "../domains/auth/authorization";
import type {
  ModelMetadata,
  ModelMetadataList,
} from "../domains/models/model-metadata";
import type { Attachment } from "./attachments";

export function discardLegacyFragmentCredential({
  location,
  history,
}: {
  location: Pick<Location, "hash" | "pathname" | "search">;
  history: Pick<History, "state" | "replaceState">;
} = window): void {
  if (!/^#key(?:=|$)/.test(location.hash)) return;
  history.replaceState(history.state, "", location.pathname + location.search);
}

type MetadataCapabilities = NonNullable<
  ModelMetadata["catalog"]["capabilities"]
>;
type SpeechCapabilities = Extract<MetadataCapabilities, { kind: "speech" }>;
type VideoMode = Extract<MetadataCapabilities, { kind: "video" }>["mode"];
// Rejecting a mode the gateway serves hides the whole catalog, so list every one.
const videoModes = [
  "t2v",
  "s2v",
  "ti2v",
] as const satisfies readonly VideoMode[];
const everyVideoMode: [VideoMode] extends [(typeof videoModes)[number]]
  ? true
  : never = true;
void everyVideoMode;
type ClientCapabilities =
  | Extract<MetadataCapabilities, { kind: "embedding" }>
  | Extract<MetadataCapabilities, { kind: "decision" }>
  | Extract<MetadataCapabilities, { kind: "video" }>
  | (Pick<SpeechCapabilities, "kind"> & {
      voice: Pick<
        SpeechCapabilities["voice"],
        "requestValues" | "defaultRequestValue"
      >;
    });

const capabilitiesSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("decision") }),
  z.object({
    kind: z.literal("video"),
    mode: z.enum(videoModes),
    width: z.number().int().positive(),
    height: z.number().int().positive(),
    frames: z.number().int().positive(),
    fps: z.number().int().positive(),
    jobDeadlineMs: z.number().int().positive(),
    outputFormats: z.tuple([z.literal("mp4")]),
  }),
  z.object({
    kind: z.literal("embedding"),
    dimensions: z.object({
      minimum: z.number().int().positive(),
      maximum: z.number().int().positive(),
    }),
  }),
  z.object({
    kind: z.literal("speech"),
    voice: z.union([
      z.object({
        requestValues: z.array(z.enum(["default", "harbor", "willow"])).min(1),
        defaultRequestValue: z.literal("default"),
      }),
      z.object({
        requestValues: z.array(z.enum(["af_heart", "af_bella"])).min(1),
        defaultRequestValue: z.enum(["af_heart", "af_bella"]),
      }),
    ]),
  }),
]) satisfies z.ZodType<ClientCapabilities>;

/** A catalog feed annotation as the server summarizes it. */
export const feedAnnotationSchema = z.object({
  repo: z.string(),
  revision: z.string(),
  file: z.string().nullable(),
  modelId: z.string().nullable(),
  status: z.enum(["qualified", "verified"]).nullable(),
  revisionMatch: z.boolean(),
  unsupportedCapabilities: z.array(
    z.object({
      id: z.string(),
      label: z.string(),
      reason: z.string(),
      applied: z.boolean(),
    }),
  ),
  memoryPeaks: z.array(
    z.object({
      variant: z.string(),
      launchShape: z.string(),
      peakBytes: z.number(),
      measuredAt: z.string(),
      hardware: z.string(),
    }),
  ),
  recipe: z
    .object({
      modelId: z.string(),
      fileCount: z.number(),
      totalBytes: z.number(),
      files: z.array(z.object({ path: z.string(), sizeBytes: z.number() })),
    })
    .nullable(),
});
export type FeedAnnotationSummary = z.infer<typeof feedAnnotationSchema>;

const modelSchema = z.object({
  id: z.string(),
  catalog: z.object({
    name: z.string(),
    displayName: z.string().optional(),
    kind: z.enum(["llm", "image", "tts", "stt", "video"]),
    qualificationState: z.enum(["qualified", "experimental"]),
    /** Catalog trust and tier; absent on older servers. */
    trust: z.enum(["qualified", "community", "user", "modified"]).optional(),
    catalogTier: z.enum(["built-in", "remote", "user"]).optional(),
    /** The remote feed a community entry came from; absent on older servers. */
    catalogFeed: z.string().nullable().optional(),
    /** Capabilities verified unsupported; absent on older servers. */
    unsupportedCapabilities: z
      .array(z.object({ id: z.string(), reason: z.string() }))
      .optional(),
    /** What the remote catalog feed publishes for this repo; absent on older servers. */
    feedAnnotation: feedAnnotationSchema.nullable().optional(),
    /** Commercial-use classification; absent on older servers. */
    commercialStatus: z.enum(["open", "conditional", "prohibited"]).optional(),
    /** One-line description; absent on older servers. */
    summary: z.string().optional(),
    quantization: z.string(),
    /** Provenance and files; absent on older servers. */
    provider: z.string().optional(),
    source: z.string().optional(),
    revision: z.string().optional(),
    notes: z.string().optional(),
    artifacts: z
      .array(
        z.object({
          role: z.string(),
          filename: z.string(),
          sizeBytes: z.number().nullable(),
        }),
      )
      .optional(),
    memory: z.object({
      minimumVramEstimateGb: z.number().nonnegative(),
      storageEstimateGb: z.number().positive(),
      /** Where the minimum came from; absent on older servers. */
      source: z.enum(["estimate", "feed", "measured", "observed"]).optional(),
      /** The catalog's own figure and its launch shape; absent on older servers. */
      reference: z
        .object({
          source: z.enum(["estimate", "feed", "measured"]),
          bytes: z.number(),
          launchShape: z.string(),
        })
        .optional(),
      /** Peaks seen on this machine, one per launch shape. */
      observedPeaks: z
        .array(
          z.object({
            launchShape: z.string(),
            peakBytes: z.number(),
            observedAt: z.string(),
          }),
        )
        .optional(),
      /** The catalog-feed peak behind the figure; absent on older servers. */
      feedPeak: z
        .object({
          peakBytes: z.number(),
          launchShape: z.string(),
          variant: z.string(),
          hardware: z.string(),
          measuredAt: z.string(),
        })
        .nullable()
        .optional(),
      /** This machine's fit verdict; absent on older servers. */
      fit: z
        .object({
          verdict: z.enum(["fits", "tight", "wont-fit"]),
          systemMemoryGb: z.number().optional(),
          headroomGb: z.number().optional(),
        })
        .passthrough()
        .optional(),
    }),
    features: z.array(z.string()).default([]),
    inputModalities: z.array(z.string()),
    outputModalities: z.array(z.string()),
    contextWindowTokens: z.number().nullable(),
    capabilities: capabilitiesSchema.nullable(),
    /** Effective capabilities with their source; absent on older servers. */
    capabilityResolution: z
      .object({
        capabilities: z.array(
          z.object({
            id: z.string(),
            source: z.string(),
          }),
        ),
        /** Sources that disagree about a capability. */
        mismatches: z
          .array(
            z.object({
              id: z.string(),
              declared: z.boolean(),
              detected: z.boolean().optional(),
              runtime: z.boolean().optional(),
              verified: z.boolean().optional(),
              effective: z.boolean(),
              unsupportedReason: z.string().optional(),
            }),
          )
          .optional(),
        inputModalities: z.object({ values: z.array(z.string()) }).optional(),
      })
      .optional(),
  }),
  device: z.object({
    selected: z.boolean(),
    installed: z.boolean(),
    runtime: z
      .object({ configured: z.boolean(), state: z.string() })
      .nullable(),
  }),
});
export type Model = z.infer<typeof modelSchema>;

/**
 * Whether the model supports this capability. Prefers the server's
 * effective set (declared, detected, or runtime) and falls back to the
 * declared features from older servers.
 */
export function modelHasCapability(
  model: Pick<Model, "catalog">,
  capability: Capability,
): boolean {
  const resolution = model.catalog.capabilityResolution;
  if (resolution)
    return resolution.capabilities.some(({ id }) => id === capability);
  return model.catalog.features.includes(capability);
}

/** Effective input modalities: detected or runtime values win over the declared list. */
export function modelInputModalities(
  model: Pick<Model, "catalog">,
): readonly string[] {
  return (
    model.catalog.capabilityResolution?.inputModalities?.values ??
    model.catalog.inputModalities
  );
}
/** Whether the model accepts the given input, using effective modalities. */
export function modelAcceptsInput(
  model: Pick<Model, "catalog">,
  modality: string,
): boolean {
  return modelInputModalities(model).includes(modality);
}
/** Input and output modalities, counting image editing as an image input. */
export function modelModalities(model: Pick<Model, "catalog">) {
  const input = [...modelInputModalities(model)];
  if (modelHasCapability(model, "image-to-image") && !input.includes("image"))
    input.push("image");
  return `${input.join(", ")} → ${model.catalog.outputModalities.join(", ")}`;
}
/** The catalog display name, falling back to the family for older servers. */
export function modelName(model: Pick<Model, "catalog">): string {
  return model.catalog.displayName ?? model.catalog.name;
}
type MetadataIdentity = Pick<ModelMetadata, "id">;
type HostMemory = ModelMetadataList["host"]["memory"];
const memoryPoolSchema = z.object({
  capacityBytes: z.number().int().nonnegative(),
  availableBytes: z.number().int().nonnegative().nullable(),
});
const hostMemorySchema = z.object({
  kind: z.enum(["unified", "discrete"]),
  system: memoryPoolSchema,
  accelerators: z.array(memoryPoolSchema),
}) satisfies z.ZodType<HostMemory>;
export const modelsSchema = z.object({
  host: z.object({ memory: hostMemorySchema }),
  data: z.array(modelSchema),
});
export type ModelsResponse = z.infer<typeof modelsSchema>;

export function modelMemorySummary(
  model: Model,
  memory: HostMemory | null,
): string {
  const availablePool =
    memory?.kind === "unified"
      ? memory.system
      : memory?.accelerators.length === 1
        ? memory.accelerators[0]
        : undefined;
  const availableBytes = availablePool?.availableBytes;
  const available =
    availableBytes == null
      ? "memory availability unknown"
      : `${Math.round((availableBytes / 1024 ** 3) * 10) / 10} GB available`;
  return `Needs ~${modelMemoryRequirement(model)} · ${available}`;
}

const memorySourceLabels = {
  estimate: "estimate",
  measured: "measured",
  observed: "observed on this machine",
  feed: "published measurement",
} as const;

const fitVerdictLabels = {
  fits: "fits",
  tight: "tight fit",
  "wont-fit": "won't fit",
} as const;

/** The minimum memory with its source, such as `3 GB (observed on this machine)`. */
export function modelMemoryRequirement(model: Model): string {
  const memory = model.catalog.memory;
  const source = memory.source ? ` (${memorySourceLabels[memory.source]})` : "";
  return `${memory.minimumVramEstimateGb} GB${source}`;
}

/** This machine's fit verdict, when the server reports one. */
export function modelFitLabel(model: Model): string | null {
  const verdict = model.catalog.memory.fit?.verdict;
  return verdict ? fitVerdictLabels[verdict] : null;
}
export const readinessSchema = z.object({
  status: z.enum(["ready", "unready"]),
  modalities: z.array(z.string()),
});
export const modes = [
  "llm",
  "image",
  "tts",
  "stt",
  "video",
  "embedding",
] as const;
export type Mode = (typeof modes)[number];
const modePermissions = {
  llm: "inference:chat",
  image: "inference:image",
  tts: "inference:speech",
  stt: "inference:transcription",
  video: "inference:video",
  embedding: "inference:embeddings",
} as const satisfies Record<Mode, Permission>;

export function hasPermission(
  permissions: readonly Permission[] | null,
  permission: Permission,
): boolean {
  return permissions?.includes(permission) === true;
}

export function canUseMode(
  permissions: readonly Permission[] | null,
  mode: Mode,
): boolean {
  return hasPermission(permissions, modePermissions[mode]);
}

export function permittedModes(
  permissions: readonly Permission[] | null,
): Mode[] {
  return modes.filter((mode) => canUseMode(permissions, mode));
}

export function permittedUiModes(
  permissions: readonly Permission[] | null,
): Mode[] {
  return hasPermission(permissions, "models:read")
    ? permittedModes(permissions)
    : [];
}
export type ToolCall = {
  id: string;
  type: "function";
  function: { name: string; arguments: string };
};
export type ChatMessage =
  | { role: "system"; content: string }
  | { role: "user"; content: string | UserContentPart[] }
  | { role: "assistant"; content: string | null; tool_calls?: ToolCall[] }
  | { role: "tool"; tool_call_id: string; content: string };
export type UserContentPart =
  | { type: "text"; text: string }
  | { type: "image_url"; image_url: { url: string } };
export type Media =
  | { kind: "image"; url: string }
  | { kind: "audio"; url: string }
  | { kind: "video"; url: string; format: "mp4" };
export type Artifact = { id: string; label: string } & (
  | { state: "working"; detail: string }
  | { state: "error"; detail: string }
  | { state: "complete"; media: Media }
);
export type Message = {
  id: string;
  role: "user" | "assistant";
  text: string;
  attachments?: Attachment[];
  attachmentsMissing?: boolean;
  media?: Media;
  artifacts?: Artifact[];
  /** Studio-only data URL of the image a result was based on, for comparison. */
  source?: string;
  protocol?: ChatMessage[];
  /** Session-only embedding vector; history keeps just the text summary. */
  embedding?: { model: string; name: string; values: number[] };
  /** The reply was cut short by Stop or by a steering message. */
  interrupted?: boolean;
  /** Model reasoning shown to the user; never sent back to the model. */
  reasoning?: string;
};
/** Older messages folded into a summary; the originals stay in `messages`. */
export type Compaction = {
  /** Id of the last message covered by the summary. */
  through: string;
  summary: string;
  method: "summary" | "dropped";
  count: number;
};
export type Conversation = {
  id: string;
  title: string;
  workspace: "chat" | "lab";
  mode: Mode;
  model: MetadataIdentity["id"];
  messages: Message[];
  compaction?: Compaction;
};
const historySchema = z
  .array(
    z.object({
      id: z.string(),
      title: z.string().max(120),
      workspace: z.enum(["chat", "lab"]),
      mode: z.enum(modes),
      model: z.string(),
      messages: z.array(
        z.object({
          id: z.string(),
          role: z.enum(["user", "assistant"]),
          text: z.string(),
          attachmentsMissing: z.boolean().optional(),
          interrupted: z.boolean().optional(),
          reasoning: z.string().max(200000).optional(),
        }),
      ),
      compaction: z
        .object({
          through: z.string(),
          summary: z.string(),
          method: z.enum(["summary", "dropped"]),
          count: z.number().int().min(0),
        })
        .optional(),
    }),
  )
  .max(30);
export const historyKey = "localbase.playground.history.v1";
export function readHistory(): Conversation[] | null {
  const raw = localStorage.getItem(historyKey);
  return raw === null ? null : historySchema.parse(JSON.parse(raw));
}
// Keeps device-local history well under the localStorage quota.
const storedReasoningLimit = 20_000;
export function writeHistory(conversations: Conversation[]) {
  // Media and credentials are deliberately excluded from device-local history.
  localStorage.setItem(
    historyKey,
    JSON.stringify(
      conversations.slice(0, 30).map((c) => ({
        id: c.id,
        title: c.title,
        workspace: c.workspace,
        mode: c.mode,
        model: c.model,
        messages: c.messages.map(
          ({
            id,
            role,
            text,
            attachments,
            attachmentsMissing,
            interrupted,
            reasoning,
          }) => ({
            id,
            role,
            text,
            ...(attachments?.length || attachmentsMissing
              ? { attachmentsMissing: true }
              : {}),
            ...(interrupted ? { interrupted } : {}),
            ...(reasoning
              ? {
                  reasoning:
                    reasoning.length > storedReasoningLimit
                      ? `${reasoning.slice(0, storedReasoningLimit)}…`
                      : reasoning,
                }
              : {}),
          }),
        ),
        ...(c.compaction ? { compaction: c.compaction } : {}),
      })),
    ),
  );
}
export function catalogModels(models: Model[], mode: Mode) {
  return models
    .filter((m) =>
      mode === "embedding"
        ? m.catalog.capabilities?.kind === "embedding"
        : m.catalog.kind === mode &&
          m.catalog.capabilities?.kind !== "embedding" &&
          m.catalog.capabilities?.kind !== "decision",
    )
    .sort(
      (a, b) =>
        Number(b.device.installed && b.device.selected) -
          Number(a.device.installed && a.device.selected) ||
        Number(b.device.installed) - Number(a.device.installed) ||
        Number(Boolean(b.device.runtime?.configured)) -
          Number(Boolean(a.device.runtime?.configured)),
    );
}
export function availableModels(models: Model[], mode: Mode) {
  return catalogModels(models, mode).filter(
    (model) => model.device.installed && model.device.selected,
  );
}
export type Connection = {
  kind: "session";
  permissions?: readonly Permission[];
  verifiedEmail?: string;
  logoutUrl?: "/cdn-cgi/access/logout";
};
export type Session = Connection & {
  permissions: readonly Permission[];
};
export type SessionState =
  Session | { kind: "checking" } | { kind: "error"; message: string };
export class SessionRequiredError extends Error {
  constructor(message = "Your sign-in has expired.") {
    super(message);
  }
}
const sessionSchema = z.object({
  authenticated: z.literal(true),
  permissions: permissionsSchema,
  verifiedEmail: z.email().optional(),
  logoutUrl: z.literal("/cdn-cgi/access/logout").optional(),
});
export async function readSession(signal?: AbortSignal): Promise<Session> {
  const response = await fetch("/app/session", {
    signal,
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    headers: { "x-localbase-ui": "1" },
  });
  if (response.status === 401 || response.status === 403) {
    await response.body?.cancel();
    throw new SessionRequiredError();
  }
  if (!response.ok) {
    await response.body?.cancel();
    throw new Error("Sign-in could not be verified. Refresh to try again.");
  }
  const parsed = sessionSchema.safeParse(
    await response.json().catch(() => null),
  );
  if (!parsed.success)
    throw new Error(
      "Sign-in could not be verified. Reload this page to sign in.",
    );
  return {
    kind: "session",
    permissions: parsed.data.permissions,
    ...(parsed.data.verifiedEmail
      ? { verifiedEmail: parsed.data.verifiedEmail }
      : {}),
    ...(parsed.data.logoutUrl ? { logoutUrl: parsed.data.logoutUrl } : {}),
  };
}
const signInRedirectKey = "localbase.signInRedirectAt";
const signInRedirectCooldownMs = 60_000;
// Sends an expired session straight to sign-in. Returns false instead when a
// sign-in just failed or was attempted moments ago, so a provider that cannot
// complete sign-in shows a message rather than redirecting in a loop.
export function redirectToSignIn(): boolean {
  if (new URLSearchParams(location.search).has("signin")) return false;
  try {
    const last = Number(sessionStorage.getItem(signInRedirectKey));
    if (Date.now() - last < signInRedirectCooldownMs) return false;
    sessionStorage.setItem(signInRedirectKey, String(Date.now()));
  } catch {
    return false;
  }
  location.assign("/app/login");
  return true;
}
export function signInFailed(): boolean {
  return new URLSearchParams(location.search).get("signin") === "failed";
}
export async function logoutSession(): Promise<void> {
  const response = await fetch("/app/logout", {
    method: "POST",
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    headers: { "x-localbase-ui": "1" },
  });
  if (!response.ok) {
    await response.body?.cancel();
    throw new Error("Could not sign out. Try again.");
  }
  await response.body?.cancel();
}
export function sessionConnection(session: SessionState): Connection | null {
  return session.kind === "session" ? session : null;
}
/** A gateway error that keeps its HTTP status and machine-readable code. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export async function api(
  path: string,
  _connection: Connection,
  init: RequestInit = {},
) {
  if (
    !path.startsWith("/") ||
    path.startsWith("//") ||
    path.includes("\\") ||
    /[\s?#%]/.test(path) ||
    path.split("/").some((part) => part === "." || part === "..")
  )
    throw new Error("Only same-origin gateway requests are allowed.");
  const headers = new Headers(init.headers);
  headers.delete("authorization");
  headers.delete("x-api-key");
  headers.set("x-localbase-ui", "1");
  let response: Response;
  try {
    response = await fetch(`/app/api${path}`, {
      ...init,
      headers,
      cache: "no-store",
      credentials: "same-origin",
      redirect: "error",
    });
  } catch (error) {
    if (!init.signal?.aborted)
      throw new Error(
        "Could not reach the gateway. Check the connection and try again.",
      );
    throw error;
  }
  if (!response.ok) {
    const value: unknown = await response.json().catch(() => null);
    const parsed = z
      .object({
        error: z.object({ message: z.string(), code: z.string().optional() }),
      })
      .safeParse(value);
    if (response.status === 401) {
      throw new SessionRequiredError();
    }
    throw new ApiError(
      parsed.success
        ? parsed.data.error.message
        : `Request failed (${response.status}). Try again.`,
      response.status,
      parsed.success ? parsed.data.error.code : undefined,
    );
  }
  if (response.headers.get("content-type")?.includes("text/html")) {
    await response.body?.cancel();
    throw new Error(
      "The gateway returned an unexpected HTML response. Reload this page or try again.",
    );
  }
  return response;
}
const chunkSchema = z.object({
  // Usage-only and keep-alive chunks may omit choices or a choice's delta.
  choices: z
    .array(
      z.object({
        index: z.number().int().optional(),
        finish_reason: z.string().nullable().optional(),
        delta: z
          .object({
            content: z.string().nullable().optional(),
            reasoning: z.string().nullable().optional(),
            reasoning_content: z.string().nullable().optional(),
            tool_calls: z
              .array(
                z.object({
                  index: z.number().int().min(0).max(3),
                  id: z.string().optional(),
                  type: z.literal("function").optional(),
                  function: z
                    .object({
                      name: z.string().optional(),
                      arguments: z.string().optional(),
                    })
                    .optional(),
                }),
              )
              .optional(),
          })
          .optional(),
      }),
    )
    .default([]),
});
/**
 * How long to wait for `[DONE]` (or the connection closing) after the model
 * reported a finish reason. A proxy that holds the stream tail must not leave
 * a finished reply spinning forever.
 */
export const streamFinishGraceMs = 2000;
export async function streamText(
  response: Response,
  append: (text: string) => void,
  reasoning?: (text: string) => void,
) {
  if (!response.body)
    throw new Error("The gateway returned an empty response.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let done = false;
  let finished = false;
  const calls = new Map<number, ToolCall>();
  const splitter = thinkSplitter();
  function emit(part: { content: string; reasoning: string }) {
    if (part.reasoning) reasoning?.(part.reasoning);
    if (part.content) append(part.content);
  }
  function event(raw: string) {
    const data = raw
      .split("\n")
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trimStart())
      .join("\n");
    if (!data) return;
    if (data === "[DONE]") {
      done = true;
      return;
    }
    const value: unknown = JSON.parse(data);
    const error = z
      .object({ error: z.object({ message: z.string() }) })
      .safeParse(value);
    if (error.success) throw new Error(error.data.error.message);
    const chunk = chunkSchema.parse(value);
    for (const choice of chunk.choices) {
      if (choice.index !== undefined && choice.index !== 0) continue;
      if (choice.finish_reason) finished = true;
      if (!choice.delta) continue;
      const thought = deltaReasoning(choice.delta);
      if (thought) reasoning?.(thought);
      if (choice.delta.content) emit(splitter.push(choice.delta.content));
      for (const delta of choice.delta.tool_calls ?? []) {
        const call = calls.get(delta.index) ?? {
          id: "",
          type: "function",
          function: { name: "", arguments: "" },
        };
        call.id += delta.id ?? "";
        call.function.name += delta.function?.name ?? "";
        call.function.arguments += delta.function?.arguments ?? "";
        if (
          call.id.length > 256 ||
          call.function.name.length > 128 ||
          call.function.arguments.length > 20000
        )
          throw new Error("Tool call exceeds browser limits.");
        calls.set(delta.index, call);
      }
    }
  }
  try {
    while (!done) {
      const read = reader.read();
      let grace: ReturnType<typeof setTimeout> | undefined;
      const part = finished
        ? await Promise.race([
            read,
            new Promise<null>((resolve) => {
              grace = setTimeout(() => resolve(null), streamFinishGraceMs);
            }),
          ]).finally(() => clearTimeout(grace))
        : await read;
      if (part === null) {
        // Finished, but the stream tail never arrived: treat it as complete.
        read.catch(() => {});
        done = true;
        break;
      }
      buffer += decoder.decode(part.value, { stream: !part.done });
      buffer = buffer.replace(/\r\n/g, "\n");
      let boundary: number;
      while ((boundary = buffer.indexOf("\n\n")) !== -1) {
        event(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
      }
      if (part.done) {
        if (buffer.trim()) event(buffer);
        break;
      }
    }
    emit(splitter.flush());
    if (!done)
      throw new Error(
        "Connection ended before the response finished. You can retry.",
      );
    const result = [...calls.entries()]
      .sort(([a], [b]) => a - b)
      .map(([, call]) => call);
    if (
      result.some((call) => !call.id || !call.function.name) ||
      new Set(result.map((call) => call.id)).size !== result.length
    )
      throw new Error(
        "Malformed tool call: missing or duplicate ID or missing function name.",
      );
    return result;
  } finally {
    // Never let cleanup of a finished (or aborted) stream block the caller.
    void reader.cancel().catch(() => {});
    try {
      reader.releaseLock();
    } catch {}
  }
}
export const imageResponseSchema = z.object({
  data: z.array(z.object({ b64_json: z.string().min(1) })).min(1),
});
export const transcriptionSchema = z.object({ text: z.string() });

export type EmbeddingSummary = {
  dimensions: number;
  preview: number[];
  norm: number;
};
/** Dimensions, the first few values, and the L2 norm of an embedding. */
export function embeddingSummary(
  values: readonly number[],
  previewLength = 8,
): EmbeddingSummary {
  return {
    dimensions: values.length,
    preview: values.slice(0, previewLength),
    norm: Math.sqrt(values.reduce((sum, value) => sum + value * value, 0)),
  };
}
/** One-line text kept in history for an embedding result. */
export function embeddingSummaryText(model: string, values: readonly number[]) {
  const { dimensions, norm } = embeddingSummary(values);
  return `Embedding from ${model}: ${dimensions} dimensions, norm ${norm.toFixed(4)}.`;
}

/** The fenced code block language from a `language-*` class, if any. */
export function codeLanguage(className: string | undefined) {
  const language = className
    ?.split(/\s+/)
    .find((name) => name.startsWith("language-"))
    ?.slice("language-".length);
  return language || undefined;
}

/** How many voices a speech model accepts; 0 for other models. */
export function speechVoiceCount(model: Pick<Model, "catalog"> | undefined) {
  const cap = model?.catalog.capabilities;
  return cap?.kind === "speech" ? cap.voice.requestValues.length : 0;
}
