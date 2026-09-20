import { z } from "zod";
import type {
  ModelMetadata,
  ModelMetadataList,
} from "../domains/models/model-metadata";
import type { Attachment } from "./attachments";

const fragmentKeySchema = z.string().regex(/^[\x21-\x7e]+$/);

export function consumeFragmentKey({
  location,
  history,
}: {
  location: Pick<Location, "hash" | "pathname" | "search">;
  history: Pick<History, "state" | "replaceState">;
} = window): string {
  const fragment = location.hash;
  if (!fragment) return "";
  // Clear even invalid credentials before session or API requests can start.
  history.replaceState(history.state, "", location.pathname + location.search);
  const match = /^#key=([^&]*)$/.exec(fragment);
  if (!match) return "";
  try {
    const key = fragmentKeySchema.safeParse(decodeURIComponent(match[1]));
    return key.success ? key.data : "";
  } catch {
    return "";
  }
}

export function createUiId(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  // HTTP LAN pages expose getRandomValues but may not expose randomUUID.
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

type MetadataCapabilities = NonNullable<
  ModelMetadata["catalog"]["capabilities"]
>;
type SpeechCapabilities = Extract<MetadataCapabilities, { kind: "speech" }>;
type ClientCapabilities =
  | Extract<MetadataCapabilities, { kind: "embedding" }>
  | Extract<MetadataCapabilities, { kind: "video" }>
  | (Pick<SpeechCapabilities, "kind"> & {
      voice: Pick<
        SpeechCapabilities["voice"],
        "requestValues" | "defaultRequestValue"
      >;
    });

const capabilitiesSchema = z.discriminatedUnion("kind", [
  z.object({
    kind: z.literal("video"),
    mode: z.enum(["t2v", "s2v"]),
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
    voice: z.object({
      requestValues: z.array(z.enum(["default", "harbor", "willow"])).min(1),
      defaultRequestValue: z.literal("default"),
    }),
  }),
]) satisfies z.ZodType<ClientCapabilities>;

const modelSchema = z.object({
  id: z.string(),
  catalog: z.object({
    name: z.string(),
    kind: z.enum(["llm", "image", "tts", "stt", "video"]),
    quantization: z.string(),
    memory: z.object({
      minimumVramEstimateGb: z.number().nonnegative(),
      unifiedMemoryEstimateGb: z.number().positive().nullable().default(null),
      storageEstimateGb: z.number().positive(),
    }),
    features: z.array(z.string()).default([]),
    inputModalities: z.array(z.string()),
    outputModalities: z.array(z.string()),
    contextWindowTokens: z.number().nullable(),
    capabilities: capabilitiesSchema.nullable(),
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
type MetadataIdentity = Pick<ModelMetadata, "id">;
export type HostMemory = ModelMetadataList["host"]["memory"];
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

export function modelMemoryRequirement(
  model: Model,
  memory: HostMemory | null,
): Readonly<{ label: string; gigabytes: number }> {
  if (memory?.kind === "unified") {
    return {
      label: "Est. unified memory",
      gigabytes:
        model.catalog.memory.unifiedMemoryEstimateGb ??
        model.catalog.memory.minimumVramEstimateGb,
    };
  }
  return {
    label: memory ? "Min. VRAM" : "Est. memory",
    gigabytes: model.catalog.memory.minimumVramEstimateGb,
  };
}

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
  const requirement = modelMemoryRequirement(model, memory);
  return `Needs ~${requirement.gigabytes} GB · ${available}`;
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
  protocol?: ChatMessage[];
};
export type Conversation = {
  id: string;
  title: string;
  workspace: "chat" | "lab";
  mode: Mode;
  model: MetadataIdentity["id"];
  messages: Message[];
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
        }),
      ),
    }),
  )
  .max(30);
export const historyKey = "localbase.playground.history.v1";
export function readHistory(): Conversation[] | null {
  const raw = localStorage.getItem(historyKey);
  return raw === null ? null : historySchema.parse(JSON.parse(raw));
}
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
          ({ id, role, text, attachments, attachmentsMissing }) => ({
            id,
            role,
            text,
            ...(attachments?.length || attachmentsMissing
              ? { attachmentsMissing: true }
              : {}),
          }),
        ),
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
          m.catalog.capabilities?.kind !== "embedding",
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
export type Session = { kind: "session" } | { kind: "api-key" };
export type SessionState =
  Session | { kind: "checking" } | { kind: "error"; message: string };
export type Connection = { kind: "session" } | { kind: "api-key"; key: string };
export class SessionRequiredError extends Error {
  constructor(
    message = "Your sign-in is missing or expired. Sign in again, then refresh.",
  ) {
    super(message);
  }
}
const sessionSchema = z.discriminatedUnion("authenticated", [
  z.object({ authenticated: z.literal(true) }),
  z.object({ authenticated: z.literal(false), mode: z.literal("api-key") }),
]);
export async function readSession({
  signal,
  key = "",
}: { signal?: AbortSignal; key?: string } = {}): Promise<Session> {
  const headers = new Headers({ "x-localbase-ui": "1" });
  const presentedKey = key.trim();
  if (presentedKey) headers.set("authorization", `Bearer ${presentedKey}`);
  const response = await fetch("/app/session", {
    method: presentedKey ? "POST" : "GET",
    signal,
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    headers,
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
  return { kind: parsed.data.authenticated ? "session" : "api-key" };
}
export function sessionConnection(
  session: SessionState,
  key: string,
): Connection | null {
  if (session.kind === "checking" || session.kind === "error") return null;
  if (session.kind === "session") return session;
  return key.trim() ? { kind: "api-key", key: key.trim() } : null;
}
export async function api(
  path: string,
  connection: Connection,
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
  if (connection.kind === "session") {
    headers.delete("authorization");
    headers.delete("x-api-key");
    headers.set("x-localbase-ui", "1");
  } else if (connection.key) {
    headers.set("authorization", `Bearer ${connection.key}`);
    headers.set("x-api-key", connection.key);
  }
  let response: Response;
  try {
    response = await fetch(
      connection.kind === "session" ? `/app/api${path}` : path,
      {
        ...init,
        headers,
        cache: "no-store",
        credentials: "same-origin",
        redirect: "error",
      },
    );
  } catch (error) {
    if (connection.kind === "session" && !init.signal?.aborted)
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
    if (
      connection.kind === "session" &&
      (response.status === 401 || response.status === 403) &&
      !(
        response.status === 403 &&
        parsed.success &&
        parsed.data.error.code === "model_management_denied"
      )
    ) {
      throw new SessionRequiredError();
    }
    throw new Error(
      response.status === 401
        ? "Enter a valid gateway API key in Settings."
        : parsed.success
          ? parsed.data.error.message
          : `Request failed (${response.status}). Try again.`,
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
  choices: z.array(
    z.object({
      index: z.number().int().optional(),
      delta: z.object({
        content: z.string().nullable().optional(),
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
      }),
    }),
  ),
});
export async function streamText(
  response: Response,
  append: (text: string) => void,
) {
  if (!response.body)
    throw new Error("The gateway returned an empty response.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let done = false;
  const calls = new Map<number, ToolCall>();
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
      if (choice.delta.content) append(choice.delta.content);
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
      const part = await reader.read();
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
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
export const imageResponseSchema = z.object({
  data: z.array(z.object({ b64_json: z.string().min(1) })).min(1),
});
export const transcriptionSchema = z.object({ text: z.string() });
