import { constants } from "node:fs";
import {
  lstat,
  readdir,
  rename,
  unlink,
  type FileHandle,
} from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import {
  assertInitializedLocalBaseRoot,
  canonicalLocalBaseRoot,
} from "../../utils/root";
import {
  assertOwnedPrivateDirectory,
  ensureOwnedPrivateDirectory,
  fileIdentity,
  openOwnedRegularFile,
  readExact,
  syncOwnedPrivateDirectory,
} from "./secure-log-files";
import type { OtelRuntime } from "./otel";

export const LOG_SCHEMA_VERSION = 2 as const;
export const LOG_DIRECTORY_NAME = "logs";
export const ACTIVE_LOG_FILENAME = "events.jsonl";
export const MAX_ACTIVE_LOG_BYTES = 10 * 1024 * 1024;
export const MAX_LOG_ARCHIVES = 5;
export const MAX_LOG_QUEUE_EVENTS = 2_048;
export const LOG_FOLLOW_POLL_MS = 200;
export const DEFAULT_LOG_LIMIT = 200;
export const MAX_LOG_LIMIT = 5_000;
export const LOG_READ_CHUNK_BYTES = 64 * 1024;
export const MAX_EVENT_LINE_BYTES = 16 * 1024;
export const MAX_BOOTSTRAP_DIAGNOSTIC_BYTES = 256 * 1024;
export const BOOTSTRAP_DIAGNOSTIC_FILENAME = "bootstrap.json";
export const LOG_SINK_RETRY_BASE_MS = 250;
export const LOG_SINK_RETRY_MAX_MS = 30_000;
export const LOG_GENERATION_RETRIES = 4;
export const LOG_FOLLOW_FINGERPRINT_BYTES = 4 * 1024;

const MAX_EVENT_MESSAGE_LENGTH = 2_048;
const MAX_ATTRIBUTE_COUNT = 16;
const MAX_ATTRIBUTE_VALUE_LENGTH = 512;
const MAX_EVENT_NAME_LENGTH = 128;
const MAX_COMPONENT_LENGTH = 64;
const MAX_REQUEST_ID_LENGTH = 128;
const REDACTED = "[REDACTED]";

export const logSeveritySchema = z.enum(["debug", "info", "warn", "error"]);
export type LogSeverity = z.infer<typeof logSeveritySchema>;

export const logCategorySchema = z.enum([
  "gateway",
  "http",
  "runtime",
  "service",
  "logging",
]);
export type LogCategory = z.infer<typeof logCategorySchema>;

export const logRuntimeSchema = z.enum([
  "gateway",
  "llm",
  "stt",
  "image",
  "service",
  "cli",
]);
export type LogRuntime = z.infer<typeof logRuntimeSchema>;

const identifierSchema = z
  .string()
  .min(1)
  .max(MAX_EVENT_NAME_LENGTH)
  .regex(/^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$/);

const componentSchema = z
  .string()
  .min(1)
  .max(MAX_COMPONENT_LENGTH)
  .regex(/^[a-z][a-z0-9-]*$/);

const logAttributeValueSchema = z.union([
  z.string().max(MAX_ATTRIBUTE_VALUE_LENGTH),
  z.number().finite(),
  z.boolean(),
]);

const logAttributesSchema = z
  .record(
    z
      .string()
      .min(1)
      .max(64)
      .regex(/^[a-z][a-z0-9_.-]*$/),
    logAttributeValueSchema,
  )
  .refine((value) => Object.keys(value).length <= MAX_ATTRIBUTE_COUNT, {
    message: `must include at most ${MAX_ATTRIBUTE_COUNT} attributes`,
  });

export const logHttpMetadataSchema = z
  .object({
    method: z.enum([
      "GET",
      "POST",
      "PUT",
      "PATCH",
      "DELETE",
      "OPTIONS",
      "HEAD",
    ]),
    path: z.enum([
      "/health",
      "/v1/models",
      "/v1/chat/completions",
      "/v1/embeddings",
      "/v1/audio/transcriptions",
      "/v1/audio/translations",
      "/v1/images/generations",
      "unmatched-route",
    ]),
    status: z.number().int().min(100).max(599),
    durationMs: z.number().finite().min(0).max(3_600_000),
  })
  .strict();

export const logErrorMetadataSchema = z
  .object({
    type: z.string().min(1).max(128),
    message: z.string().min(1).max(MAX_EVENT_MESSAGE_LENGTH),
    code: z.string().min(1).max(128).optional(),
  })
  .strict();

export const logTraceCorrelationSchema = z
  .object({
    traceId: z.string(),
    spanId: z.string(),
  })
  .strict();
export type LogTraceCorrelation = z.infer<typeof logTraceCorrelationSchema>;

/** Stable structured event contract shared by local files and future OTLP export. */
export const logEventSchema = z
  .object({
    schemaVersion: z.literal(LOG_SCHEMA_VERSION),
    id: z.uuid(),
    timestamp: z.iso.datetime({ offset: true }),
    severity: logSeveritySchema,
    eventName: identifierSchema,
    category: logCategorySchema,
    component: componentSchema,
    runtime: logRuntimeSchema,
    message: z.string().min(1).max(MAX_EVENT_MESSAGE_LENGTH),
    requestId: z
      .string()
      .min(1)
      .max(MAX_REQUEST_ID_LENGTH)
      .regex(/^[A-Za-z0-9._:-]+$/)
      .optional(),
    trace: logTraceCorrelationSchema.optional(),
    http: logHttpMetadataSchema.optional(),
    error: logErrorMetadataSchema.optional(),
    attributes: logAttributesSchema.optional(),
  })
  .strict();

export type LogEvent = z.infer<typeof logEventSchema>;

export const diagnosticsLogEventSchema = z
  .object({
    schemaVersion: z.literal(LOG_SCHEMA_VERSION),
    id: z.uuid(),
    timestamp: z.iso.datetime({ offset: true }),
    severity: logSeveritySchema,
    eventName: identifierSchema,
    category: logCategorySchema,
    component: componentSchema,
    runtime: logRuntimeSchema,
    message: z.string().min(1).max(MAX_EVENT_MESSAGE_LENGTH),
    error: z
      .object({ message: z.string().min(1).max(MAX_EVENT_MESSAGE_LENGTH) })
      .strict()
      .optional(),
    attributes: logAttributesSchema.optional(),
  })
  .strict();
export type DiagnosticsLogEvent = z.infer<typeof diagnosticsLogEventSchema>;

export type LogEventInput = {
  severity: LogSeverity;
  eventName: string;
  category: LogCategory;
  component: string;
  runtime: LogRuntime;
  message: string;
  requestId?: string;
  http?: Omit<z.input<typeof logHttpMetadataSchema>, "path"> & {
    path: string;
  };
  error?: {
    type?: unknown;
    message?: unknown;
    code?: unknown;
  };
  attributes?: Record<string, unknown>;
};

const sensitiveKeyPattern =
  /(?:authorization|bearer|api[-_. ]?key|token|secret|password|credential|cookie|hf[-_. ]?token)/i;
const contentKeyPattern =
  /(?:content|prompt|messages?|input|body|response|completion|output)/i;
const bearerPattern = /\bBearer\s+[A-Za-z0-9._~+\/-]+=*/gi;
const labeledSecretPattern =
  /\b(authorization|api[-_ ]?key|token|secret|password|hf[-_ ]?token)\s*[:=]\s*([^\s,;]+)/gi;
const knownTokenPattern =
  /\b(?:hf_[A-Za-z0-9_-]{8,}|lb_[A-Za-z0-9_-]{8,}|sk(?:-proj)?-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{12,}|AKIA[A-Z0-9]{16}|[A-Za-z0-9_-]*token[A-Za-z0-9_-]{12,})\b/gi;
const cookiePattern = /\b(?:set-cookie|cookie)\s*:\s*[^\r\n]+/gi;
const urlCredentialPattern = /([a-z][a-z0-9+.-]*:\/\/)[^@\s/]+@/gi;
const urlQuerySecretPattern =
  /([?&](?:access_token|api[-_]?key|authorization|token|secret|password)=)[^&#\s]*/gi;
const embeddedContentPattern =
  /["']?(?:content|prompt|messages?|input|body|response|completion|output)["']?\s*[:=]/i;

function boundedText(
  value: unknown,
  maximum = MAX_EVENT_MESSAGE_LENGTH,
): string {
  const text = typeof value === "string" ? value : String(value ?? "");
  if (embeddedContentPattern.test(text)) {
    return "[REDACTED REQUEST OR MODEL CONTENT]";
  }
  const redacted = text
    .replace(bearerPattern, "Bearer [REDACTED]")
    .replace(labeledSecretPattern, "$1=[REDACTED]")
    .replace(cookiePattern, "cookie: [REDACTED]")
    .replace(urlCredentialPattern, "$1[REDACTED]@")
    .replace(urlQuerySecretPattern, "$1[REDACTED]")
    .replace(knownTokenPattern, REDACTED);
  return redacted.length <= maximum
    ? redacted
    : `${redacted.slice(0, Math.max(0, maximum - 1))}…`;
}

/** Redacts and bounds an external string before any diagnostic sink. */
export function redactExternalLogText(value: unknown, maximum = 512): string {
  return boundedText(value, maximum);
}

function diagnosticIdentifier(
  value: string,
  fallback: string,
  maximum: number,
): string {
  const redacted = redactExternalLogText(value, maximum);
  return redacted.includes(REDACTED) ? fallback : redacted;
}

export function redactLogEventForDiagnostics(
  event: LogEvent,
): DiagnosticsLogEvent {
  return diagnosticsLogEventSchema.parse({
    schemaVersion: event.schemaVersion,
    id: event.id,
    timestamp: event.timestamp,
    severity: event.severity,
    eventName: diagnosticIdentifier(
      event.eventName,
      "diagnostics.event",
      MAX_EVENT_NAME_LENGTH,
    ),
    category: event.category,
    component: diagnosticIdentifier(
      event.component,
      "diagnostics",
      MAX_COMPONENT_LENGTH,
    ),
    runtime: event.runtime,
    message: redactExternalLogText(event.message, MAX_EVENT_MESSAGE_LENGTH),
    ...(event.error
      ? {
          error: {
            message: redactExternalLogText(
              event.error.message,
              MAX_EVENT_MESSAGE_LENGTH,
            ),
          },
        }
      : {}),
    ...(event.attributes
      ? { attributes: redactLogAttributes(event.attributes) }
      : {}),
  });
}

function normalizedComponent(value: string): string {
  const normalized = value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, MAX_COMPONENT_LENGTH)
    .replace(/-+$/g, "");
  return normalized && /^[a-z]/.test(normalized) ? normalized : "localbase";
}

function normalizedEventName(value: string): string {
  const normalized = value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^[._-]+|[._-]+$/g, "")
    .slice(0, MAX_EVENT_NAME_LENGTH)
    .replace(/[._-]+$/g, "");
  return normalized && /^[a-z]/.test(normalized)
    ? normalized
    : "localbase.event";
}

function safeRequestId(value: string | undefined): string | undefined {
  if (!value) return undefined;
  let decoded: string;
  try {
    decoded = decodeURIComponent(value);
  } catch {
    return undefined;
  }
  const redacted = boundedText(decoded, MAX_REQUEST_ID_LENGTH);
  const unsafe =
    redacted === REDACTED ||
    redacted.includes(REDACTED) ||
    sensitiveKeyPattern.test(decoded) ||
    bearerPattern.test(decoded) ||
    knownTokenPattern.test(decoded);
  bearerPattern.lastIndex = 0;
  knownTokenPattern.lastIndex = 0;
  if (unsafe) return undefined;
  const parsed = logEventSchema.shape.requestId.safeParse(redacted);
  return parsed.success ? parsed.data : undefined;
}

function recognizedHttpRoute(
  value: string,
): z.infer<typeof logHttpMetadataSchema>["path"] {
  const pathname = value.split("?", 1)[0];
  const parsed = logHttpMetadataSchema.shape.path.safeParse(pathname);
  return parsed.success ? parsed.data : "unmatched-route";
}

/** Redacts values before an event reaches either console or file sinks. */
export function redactLogAttributes(
  attributes: Record<string, unknown> | undefined,
): Record<string, z.infer<typeof logAttributeValueSchema>> | undefined {
  if (!attributes) return undefined;
  const result: Record<string, z.infer<typeof logAttributeValueSchema>> = {};
  for (const [key, value] of Object.entries(attributes)) {
    if (Object.keys(result).length >= MAX_ATTRIBUTE_COUNT) break;
    if (!/^[a-z][a-z0-9_.-]*$/i.test(key) || key.length > 64) continue;
    const normalizedKey = key.toLowerCase();
    if (sensitiveKeyPattern.test(key) || contentKeyPattern.test(key)) {
      result[normalizedKey] = REDACTED;
      continue;
    }
    if (typeof value === "string") {
      result[normalizedKey] = boundedText(value, MAX_ATTRIBUTE_VALUE_LENGTH);
    } else if (typeof value === "boolean") {
      result[normalizedKey] = value;
    } else if (typeof value === "number" && Number.isFinite(value)) {
      result[normalizedKey] = value;
    }
  }
  return Object.keys(result).length > 0 ? result : undefined;
}

/** Creates one validated, redacted event; sinks never receive unvalidated data. */
export function createLogEvent(
  input: LogEventInput,
  trace?: LogTraceCorrelation,
): LogEvent {
  const error = input.error
    ? {
        type: boundedText(input.error.type || "Error", 128),
        message: boundedText(input.error.message || "Unknown error"),
        ...(typeof input.error.code === "string" && input.error.code
          ? { code: boundedText(input.error.code, 128) }
          : {}),
      }
    : undefined;
  const parsedHttp = input.http
    ? logHttpMetadataSchema.safeParse({
        ...input.http,
        path: recognizedHttpRoute(String(input.http.path)),
      })
    : undefined;
  const requestId = safeRequestId(input.requestId);
  const attributes = redactLogAttributes(input.attributes);
  return logEventSchema.parse({
    schemaVersion: LOG_SCHEMA_VERSION,
    id: crypto.randomUUID(),
    timestamp: new Date().toISOString(),
    severity: input.severity,
    eventName: normalizedEventName(input.eventName),
    category: input.category,
    component: normalizedComponent(input.component),
    runtime: input.runtime,
    message: boundedText(input.message),
    ...(requestId ? { requestId } : {}),
    ...(trace ? { trace } : {}),
    ...(parsedHttp?.success ? { http: parsedHttp.data } : {}),
    ...(error ? { error } : {}),
    ...(attributes ? { attributes } : {}),
  });
}

function runtimeForComponent(component: string): LogRuntime {
  const normalized = normalizedComponent(component);
  if (normalized.includes("llama") || normalized === "llm") return "llm";
  if (normalized.includes("whisper") || normalized === "stt") return "stt";
  if (normalized.includes("sd-") || normalized === "image") return "image";
  if (normalized.includes("service")) return "service";
  if (normalized.includes("cli") || normalized === "sync") return "cli";
  return "gateway";
}

function consoleWrite(event: LogEvent, format: "human" | "json"): void {
  if (format === "json") {
    console.log(JSON.stringify(event));
    return;
  }
  const color =
    event.severity === "error"
      ? "\x1b[31m"
      : event.severity === "warn"
        ? "\x1b[33m"
        : event.severity === "debug"
          ? "\x1b[90m"
          : "\x1b[32m";
  const line = `[${event.timestamp}] ${color}[${event.severity.toUpperCase()}]\x1b[0m [\x1b[36m${event.component}\x1b[0m] ${event.message}`;
  if (event.severity === "error") console.error(line);
  else if (event.severity === "warn") console.warn(line);
  else console.log(line);
}

export function logDirectory(root: string): string {
  return join(canonicalLocalBaseRoot(root), LOG_DIRECTORY_NAME);
}

export function activeLogPath(root: string): string {
  return join(logDirectory(root), ACTIVE_LOG_FILENAME);
}

export function bootstrapDiagnosticPath(root: string): string {
  return join(logDirectory(root), BOOTSTRAP_DIAGNOSTIC_FILENAME);
}

const bootstrapReplacementTails = new Map<string, Promise<void>>();

async function serializeBootstrapReplacement<T>(
  root: string,
  replace: () => Promise<T>,
): Promise<T> {
  const previous = bootstrapReplacementTails.get(root) ?? Promise.resolve();
  const result = previous.catch(() => {}).then(replace);
  const tail = result.then(
    () => {},
    () => {},
  );
  bootstrapReplacementTails.set(root, tail);
  return result.finally(() => {
    if (bootstrapReplacementTails.get(root) === tail) {
      bootstrapReplacementTails.delete(root);
    }
  });
}

export async function writeBootstrapDiagnostic(
  root: string,
  error: unknown,
): Promise<LogEvent> {
  const canonical = canonicalLocalBaseRoot(root);
  assertInitializedLocalBaseRoot(canonical);
  await ensureOwnedPrivateDirectory(logDirectory(root));
  const event = createLogEvent({
    severity: "error",
    eventName: "gateway.bootstrap-failed",
    category: "gateway",
    component: "gateway",
    runtime: "gateway",
    message: "Managed gateway initialization failed.",
    error: {
      type: error instanceof Error ? error.name : "Error",
      message: error instanceof Error ? error.message : String(error),
    },
  });
  const bytes = Buffer.from(`${JSON.stringify(event)}\n`);
  if (bytes.length > MAX_BOOTSTRAP_DIAGNOSTIC_BYTES) {
    throw new Error("Bootstrap diagnostic exceeds its bounded record size.");
  }
  await serializeBootstrapReplacement(canonical, async () => {
    const directory = logDirectory(canonical);
    const destination = bootstrapDiagnosticPath(canonical);
    const temporary = join(directory, `.bootstrap.${crypto.randomUUID()}.tmp`);
    const opened = await openOwnedRegularFile(
      temporary,
      constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY,
    );
    const temporaryIdentity = fileIdentity(opened.stat);
    try {
      await opened.handle.writeFile(bytes);
      await opened.handle.sync();
    } finally {
      await opened.handle.close();
    }
    try {
      try {
        const existing = await openOwnedRegularFile(
          destination,
          constants.O_RDONLY,
        );
        await existing.handle.close();
      } catch (existingError) {
        if ((existingError as NodeJS.ErrnoException).code !== "ENOENT") {
          throw existingError;
        }
      }
      await rename(temporary, destination);
      const validated = await openOwnedRegularFile(
        destination,
        constants.O_RDONLY,
      );
      try {
        if (fileIdentity(validated.stat) !== temporaryIdentity) {
          throw new Error(
            "Bootstrap diagnostic changed during atomic replacement.",
          );
        }
      } finally {
        await validated.handle.close();
      }
      await syncOwnedPrivateDirectory(directory);
    } catch (failure) {
      await unlinkOwnedFileIfPresent(temporary).catch(() => {});
      throw failure;
    }
  });
  return event;
}

export async function clearBootstrapDiagnostic(root: string): Promise<void> {
  await unlinkOwnedFileIfPresent(bootstrapDiagnosticPath(root));
}

function archivePath(root: string, index: number): string {
  if (!Number.isInteger(index) || index < 1 || index > MAX_LOG_ARCHIVES) {
    throw new Error(`Invalid LocalBase log archive index: ${index}.`);
  }
  return join(logDirectory(root), `events.${index}.jsonl`);
}

async function unlinkOwnedFileIfPresent(path: string): Promise<void> {
  let opened: Awaited<ReturnType<typeof openOwnedRegularFile>> | undefined;
  try {
    opened = await openOwnedRegularFile(path, constants.O_RDONLY);
    await unlink(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  } finally {
    await opened?.handle.close().catch(() => {});
  }
}

export type LogAppender = (path: string, contents: string) => Promise<void>;

export type RotatingLogWriterOptions = {
  maxActiveBytes?: number;
  maxArchives?: number;
  maxQueueEvents?: number;
  append?: LogAppender;
  onFailure?: (message: string) => void;
  retryBaseMs?: number;
  retryMaxMs?: number;
  onRetryScheduled?: (delayMs: number) => void;
};

/**
 * Serializes the root's authoritative JSONL stream. Production creates exactly
 * one instance after acquiring the gateway lease; CLI readers never create one.
 */
export class RotatingLogWriter {
  private readonly root: string;
  private readonly maxActiveBytes: number;
  private readonly maxArchives: number;
  private readonly maxQueueEvents: number;
  private readonly append?: LogAppender;
  private readonly onFailure: (message: string) => void;
  private readonly retryBaseMs: number;
  private readonly retryMaxMs: number;
  private readonly onRetryScheduled: (delayMs: number) => void;
  private readonly queue: LogEvent[] = [];
  private draining: Promise<void> | undefined;
  private handle: FileHandle | undefined;
  private activeIdentity: string | undefined;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private nextRetryAt = 0;
  private failures = 0;
  private closingFailureAttempts = 0;
  private dropped = 0;
  private closed = false;
  private failureReported = false;

  constructor(root: string, options: RotatingLogWriterOptions = {}) {
    this.root = canonicalLocalBaseRoot(root);
    this.maxActiveBytes = options.maxActiveBytes ?? MAX_ACTIVE_LOG_BYTES;
    this.maxArchives = options.maxArchives ?? MAX_LOG_ARCHIVES;
    this.maxQueueEvents = options.maxQueueEvents ?? MAX_LOG_QUEUE_EVENTS;
    this.append = options.append;
    this.onFailure = options.onFailure ?? (() => {});
    this.retryBaseMs = options.retryBaseMs ?? LOG_SINK_RETRY_BASE_MS;
    this.retryMaxMs = options.retryMaxMs ?? LOG_SINK_RETRY_MAX_MS;
    this.onRetryScheduled = options.onRetryScheduled ?? (() => {});
    if (
      !Number.isInteger(this.maxActiveBytes) ||
      this.maxActiveBytes < 1 ||
      !Number.isInteger(this.maxArchives) ||
      this.maxArchives < 1 ||
      !Number.isInteger(this.maxQueueEvents) ||
      this.maxQueueEvents < 1
    ) {
      throw new Error(
        "LocalBase log writer options must be positive integers.",
      );
    }
  }

  async open(): Promise<void> {
    assertInitializedLocalBaseRoot(this.root);
    await ensureOwnedPrivateDirectory(logDirectory(this.root));
    await this.openActive();
  }

  enqueue(event: LogEvent): void {
    if (this.closed) return;
    if (this.queue.length >= this.maxQueueEvents) {
      this.dropped += 1;
      return;
    }
    this.queue.push(logEventSchema.parse(event));
    this.enqueueDrain();
  }

  private enqueueDrain(): void {
    if (this.draining || this.retryTimer) return;
    if (Date.now() < this.nextRetryAt) {
      this.scheduleRecovery();
      return;
    }
    this.draining = this.drain().finally(() => {
      this.draining = undefined;
      if (!this.closed && this.queue.length > 0) this.enqueueDrain();
    });
  }

  private scheduleRecovery(): void {
    if (this.closed || this.retryTimer || this.queue.length === 0) return;
    const delay = Math.max(0, this.nextRetryAt - Date.now());
    if (delay === 0) {
      this.enqueueDrain();
      return;
    }
    this.onRetryScheduled(delay);
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      this.enqueueDrain();
    }, delay);
  }

  private async openActive(): Promise<void> {
    const opened = await openOwnedRegularFile(
      activeLogPath(this.root),
      constants.O_CREAT | constants.O_RDWR | constants.O_APPEND,
    );
    this.handle = opened.handle;
    this.activeIdentity = fileIdentity(opened.stat);
    await this.repairTrailingPartialLine();
  }

  private async repairTrailingPartialLine(): Promise<void> {
    if (!this.handle) throw new Error("LocalBase log sink is not open.");
    const info = await this.handle.stat();
    if (info.size === 0) return;
    const length = Math.min(info.size, MAX_EVENT_LINE_BYTES + 1);
    const tail = await readExact(this.handle, info.size - length, length);
    const newline = tail.lastIndexOf(0x0a);
    if (newline === tail.length - 1) return;
    const safeSize = newline < 0 ? 0 : info.size - length + newline + 1;
    await this.handle.truncate(safeSize);
    await this.handle.sync();
  }

  private async validateActiveHandle(): Promise<number> {
    if (!this.handle || !this.activeIdentity) await this.openActive();
    const info = await this.handle!.stat();
    const pathInfo = await lstat(activeLogPath(this.root));
    if (
      !pathInfo.isFile() ||
      pathInfo.isSymbolicLink() ||
      fileIdentity(info) !== this.activeIdentity ||
      fileIdentity(pathInfo) !== this.activeIdentity
    ) {
      throw new Error(
        "LocalBase active log path no longer names its open file.",
      );
    }
    return info.size;
  }

  private async rotateFor(incomingBytes: number): Promise<void> {
    const active = activeLogPath(this.root);
    const size = await this.validateActiveHandle();
    if (size === 0 || size + incomingBytes <= this.maxActiveBytes) return;
    await this.handle!.sync();
    await this.handle!.close();
    this.handle = undefined;
    this.activeIdentity = undefined;
    await unlinkOwnedFileIfPresent(archivePath(this.root, this.maxArchives));
    for (let index = this.maxArchives - 1; index >= 1; index -= 1) {
      const source = archivePath(this.root, index);
      let opened: Awaited<ReturnType<typeof openOwnedRegularFile>> | undefined;
      try {
        opened = await openOwnedRegularFile(source, constants.O_RDONLY);
        const destination = archivePath(this.root, index + 1);
        await rename(source, destination);
        const moved = await openOwnedRegularFile(
          destination,
          constants.O_RDONLY,
        );
        try {
          if (fileIdentity(moved.stat) !== fileIdentity(opened.stat)) {
            throw new Error("LocalBase log archive changed during rotation.");
          }
        } finally {
          await moved.handle.close();
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      } finally {
        await opened?.handle.close().catch(() => {});
      }
    }
    const activeCheck = await openOwnedRegularFile(active, constants.O_RDONLY);
    const firstArchive = archivePath(this.root, 1);
    try {
      await rename(active, firstArchive);
      const moved = await openOwnedRegularFile(
        firstArchive,
        constants.O_RDONLY,
      );
      try {
        if (fileIdentity(moved.stat) !== fileIdentity(activeCheck.stat)) {
          throw new Error("LocalBase active log changed during rotation.");
        }
      } finally {
        await moved.handle.close();
      }
    } finally {
      await activeCheck.handle.close();
    }
    await this.openActive();
  }

  private async writeEvent(event: LogEvent): Promise<void> {
    const line = `${JSON.stringify(logEventSchema.parse(event))}\n`;
    const bytes = Buffer.from(line);
    if (bytes.byteLength > MAX_EVENT_LINE_BYTES) {
      throw new Error("Structured log event exceeds the bounded line size.");
    }
    await this.rotateFor(bytes.byteLength);
    if (this.append) {
      await this.append(activeLogPath(this.root), line);
      return;
    }
    if (!this.handle) throw new Error("LocalBase log sink is not open.");
    let offset = 0;
    while (offset < bytes.length) {
      const result = await this.handle.write(
        bytes,
        offset,
        bytes.length - offset,
      );
      if (result.bytesWritten === 0) {
        throw new Error("LocalBase log sink made no write progress.");
      }
      offset += result.bytesWritten;
    }
  }

  private reportFailure(error: unknown): void {
    if (this.failureReported) return;
    this.failureReported = true;
    this.onFailure(
      `LocalBase file logging is unavailable: ${boundedText(error instanceof Error ? error.message : error, 512)}`,
    );
  }

  private async drain(): Promise<void> {
    while (this.queue.length > 0) {
      if (Date.now() < this.nextRetryAt) {
        this.scheduleRecovery();
        return;
      }
      const event = this.queue.shift()!;
      const dropped = this.dropped;
      try {
        if (dropped > 0) {
          await this.writeEvent(
            createLogEvent({
              severity: "warn",
              eventName: "logging.events-dropped",
              category: "logging",
              component: "logger",
              runtime: "gateway",
              message:
                "Dropped structured log events while the file sink was unavailable or saturated.",
              attributes: { dropped },
            }),
          );
          this.dropped = 0;
        }
        await this.writeEvent(event);
        this.failureReported = false;
        this.failures = 0;
        this.nextRetryAt = 0;
      } catch (error) {
        this.dropped += 1;
        this.failures += 1;
        this.nextRetryAt =
          Date.now() +
          Math.min(
            this.retryMaxMs,
            this.retryBaseMs * 2 ** Math.min(this.failures - 1, 16),
          );
        await this.handle?.close().catch(() => {});
        this.handle = undefined;
        this.activeIdentity = undefined;
        this.reportFailure(error);
        if (this.closed) {
          if (this.closingFailureAttempts === 0 && this.queue.length > 0) {
            this.closingFailureAttempts = 1;
            this.nextRetryAt = 0;
          } else {
            this.dropped += this.queue.length;
            this.queue.length = 0;
          }
          return;
        }
        this.scheduleRecovery();
        return;
      }
    }
  }

  async flush(): Promise<void> {
    if (this.retryTimer) {
      clearTimeout(this.retryTimer);
      this.retryTimer = undefined;
      this.nextRetryAt = 0;
      if (this.dropped > 0) {
        this.queue.unshift(
          createLogEvent({
            severity: "warn",
            eventName: "logging.events-dropped",
            category: "logging",
            component: "logger",
            runtime: "gateway",
            message:
              "Dropped structured log events while the file sink was unavailable or saturated.",
            attributes: { dropped: this.dropped },
          }),
        );
        this.dropped = 0;
      }
    }
    while (this.draining || this.queue.length > 0) {
      await this.draining;
      if (!this.draining && this.queue.length > 0) this.enqueueDrain();
    }
  }

  async close(): Promise<void> {
    this.closed = true;
    await this.flush();
    await this.handle?.sync().catch(() => {});
    await this.handle?.close().catch(() => {});
    this.handle = undefined;
  }
}

export interface ILogger {
  info(
    component: string,
    message: string,
    attributes?: Record<string, unknown>,
  ): void;
  warn(
    component: string,
    message: string,
    attributes?: Record<string, unknown>,
  ): void;
  error(
    component: string,
    message: string,
    error?: Error,
    attributes?: Record<string, unknown>,
  ): void;
  event(input: LogEventInput): void;
  localDiagnostic?(input: LogEventInput): void;
  request(
    method: string,
    path: string,
    status: number,
    durationMs: number,
    requestId?: string,
  ): void;
  pipeStream(stream: ReadableStream<Uint8Array>, component: string): void;
  enableFileLogging(root: string): Promise<void>;
  setOtelRuntime?(runtime: OtelRuntime): void;
  setConfigurationRevision?(revision: () => number): void;
  close(): Promise<void>;
}

/** Composite console and file logger for gateway operational events. */
export class LocalBaseLogger implements ILogger {
  private readonly format: "human" | "json";
  private writer: RotatingLogWriter | undefined;
  private otel: OtelRuntime | undefined;
  private configurationRevision: (() => number) | undefined;

  constructor(format?: string) {
    this.format =
      format?.toLowerCase() === "json" ||
      process.env.LOG_FORMAT?.toLowerCase() === "json"
        ? "json"
        : "human";
  }

  event(input: LogEventInput): void {
    this.writeEvent(input, true);
  }

  localDiagnostic(input: LogEventInput): void {
    this.writeEvent(input, false);
  }

  private writeEvent(input: LogEventInput, exportOtel: boolean): void {
    const revision = this.configurationRevision?.();
    const event = createLogEvent(
      {
        ...input,
        attributes:
          Number.isInteger(revision) && revision! >= 0
            ? {
                ...input.attributes,
                "localbase.config_revision": revision,
              }
            : input.attributes,
      },
      this.otel?.activeCorrelation(),
    );
    consoleWrite(event, this.format);
    this.writer?.enqueue(event);
    if (exportOtel) this.otel?.emit(event);
  }

  info(
    component: string,
    message: string,
    attributes?: Record<string, unknown>,
  ): void {
    this.event({
      severity: "info",
      eventName: "runtime.message",
      category: "runtime",
      component,
      runtime: runtimeForComponent(component),
      message,
      attributes,
    });
  }

  warn(
    component: string,
    message: string,
    attributes?: Record<string, unknown>,
  ): void {
    this.event({
      severity: "warn",
      eventName: "runtime.message",
      category: "runtime",
      component,
      runtime: runtimeForComponent(component),
      message,
      attributes,
    });
  }

  error(
    component: string,
    message: string,
    error?: Error,
    attributes?: Record<string, unknown>,
  ): void {
    this.event({
      severity: "error",
      eventName: "runtime.error",
      category: "runtime",
      component,
      runtime: runtimeForComponent(component),
      message,
      ...(error
        ? {
            error: {
              type: error.name || "Error",
              message: error.message,
            },
          }
        : {}),
      attributes,
    });
  }

  request(
    method: string,
    path: string,
    status: number,
    durationMs: number,
    requestId?: string,
  ): void {
    const parsedMethod = logHttpMetadataSchema.shape.method.safeParse(method);
    const parsedStatus = z.number().int().min(100).max(599).safeParse(status);
    if (!parsedMethod.success || !parsedStatus.success) return;
    this.event({
      severity: status >= 500 ? "error" : status >= 400 ? "warn" : "info",
      eventName: "http.request",
      category: "http",
      component: "gateway",
      runtime: "gateway",
      message: `${method} ${recognizedHttpRoute(path)} -> ${status}`,
      requestId,
      http: {
        method: parsedMethod.data,
        path: recognizedHttpRoute(path),
        status: parsedStatus.data,
        durationMs: Number(durationMs.toFixed(2)),
      },
    });
  }

  pipeStream(stream: ReadableStream<Uint8Array>, component: string): void {
    const reader = stream.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    void (async () => {
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffered += decoder.decode(value, { stream: true });
          const lines = buffered.split("\n");
          buffered = lines.pop() ?? "";
          for (const line of lines) {
            if (!line.trim()) continue;
            this.event({
              severity: "info",
              eventName: "runtime.output",
              category: "runtime",
              component,
              runtime: runtimeForComponent(component),
              message: "Backend emitted a log line.",
              attributes: { lineLength: line.length },
            });
          }
        }
      } catch {
        // Child output is best-effort operational context.
      } finally {
        reader.releaseLock();
      }
    })();
  }

  async enableFileLogging(root: string): Promise<void> {
    if (this.writer) return;
    const writer = new RotatingLogWriter(root, {
      onFailure: (message) => process.stderr.write(`${message}\n`),
    });
    await writer.open();
    this.writer = writer;
  }

  setOtelRuntime(runtime: OtelRuntime): void {
    this.otel = runtime;
  }

  setConfigurationRevision(revision: () => number): void {
    this.configurationRevision = revision;
  }

  async close(): Promise<void> {
    await this.writer?.close();
    this.writer = undefined;
    await this.otel?.shutdown();
    this.otel = undefined;
  }
}

export function createLogger(format?: string): ILogger {
  return new LocalBaseLogger(format);
}

export type LogFilters = {
  since?: string;
  level?: LogSeverity;
  runtime?: LogRuntime;
  requestId?: string;
};

export function matchesLogFilters(
  event: LogEvent,
  filters: LogFilters,
): boolean {
  if (
    filters.since &&
    Date.parse(event.timestamp) < Date.parse(filters.since)
  ) {
    return false;
  }
  if (filters.level && event.severity !== filters.level) return false;
  if (filters.runtime && event.runtime !== filters.runtime) return false;
  return !filters.requestId || event.requestId === filters.requestId;
}

async function orderedLogPaths(root: string): Promise<string[]> {
  const canonical = canonicalLocalBaseRoot(root);
  const directory = logDirectory(canonical);
  try {
    await assertOwnedPrivateDirectory(directory);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw error;
  }
  const entries = await readdir(directory);
  const archives = entries
    .map((name) => ({ name, match: /^events\.(\d+)\.jsonl$/.exec(name) }))
    .filter(
      (entry): entry is { name: string; match: RegExpExecArray } =>
        entry.match !== null &&
        Number(entry.match[1]) >= 1 &&
        Number(entry.match[1]) <= MAX_LOG_ARCHIVES,
    )
    .sort((left, right) => Number(right.match[1]) - Number(left.match[1]))
    .map((entry) => join(directory, entry.name));
  const active = activeLogPath(canonical);
  const bootstrap = bootstrapDiagnosticPath(canonical);
  let bootstrapPresent = false;
  try {
    const info = await lstat(bootstrap);
    if (!info.isFile() || info.isSymbolicLink()) {
      throw new Error(
        `LocalBase log path is not a regular file: ${bootstrap}.`,
      );
    }
    bootstrapPresent = true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  try {
    const info = await lstat(active);
    if (!info.isFile() || info.isSymbolicLink()) {
      if (bootstrapPresent) {
        return [bootstrap, ...archives];
      }
      throw new Error(`LocalBase log path is not a regular file: ${active}.`);
    }
    return [...(bootstrapPresent ? [bootstrap] : []), ...archives, active];
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return [...(bootstrapPresent ? [bootstrap] : []), ...archives];
    }
    throw error;
  }
}

type OpenLogFile = {
  path: string;
  identity: string;
  size: number;
  handle: FileHandle;
};

async function closeLogFiles(files: OpenLogFile[]): Promise<void> {
  await Promise.all(files.map((file) => file.handle.close().catch(() => {})));
}

async function generationStillCurrent(
  root: string,
  files: OpenLogFile[],
): Promise<boolean> {
  const paths = await orderedLogPaths(root);
  if (
    paths.length !== files.length ||
    paths.some((path, index) => path !== files[index]?.path)
  ) {
    return false;
  }
  for (const file of files) {
    try {
      const info = await lstat(file.path);
      if (
        !info.isFile() ||
        info.isSymbolicLink() ||
        fileIdentity(info) !== file.identity
      ) {
        return false;
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
      throw error;
    }
  }
  return true;
}

/**
 * Captures one deduplicated file generation. Rotation races retry from path
 * discovery; the final attempt still returns safe handles rather than ENOENT.
 */
async function openLogGeneration(root: string): Promise<OpenLogFile[]> {
  for (let attempt = 0; attempt < LOG_GENERATION_RETRIES; attempt += 1) {
    const files: OpenLogFile[] = [];
    const identities = new Set<string>();
    let changed = false;
    for (const path of await orderedLogPaths(root)) {
      try {
        const opened = await openOwnedRegularFile(path, constants.O_RDONLY);
        const identity = fileIdentity(opened.stat);
        if (identities.has(identity)) {
          await opened.handle.close();
          continue;
        }
        identities.add(identity);
        files.push({
          path,
          identity,
          size: opened.stat.size,
          handle: opened.handle,
        });
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
          await closeLogFiles(files);
          throw error;
        }
        changed = true;
        break;
      }
    }
    if (!changed && (await generationStillCurrent(root, files))) return files;
    if (attempt === LOG_GENERATION_RETRIES - 1) return files;
    await closeLogFiles(files);
  }
  return [];
}

function parseLogLines(contents: string): LogEvent[] {
  const events: LogEvent[] = [];
  for (const line of contents.split("\n")) {
    if (!line) continue;
    const parsed = (() => {
      try {
        return logEventSchema.safeParse(JSON.parse(line));
      } catch {
        return undefined;
      }
    })();
    if (parsed?.success) events.push(parsed.data);
  }
  return events;
}

async function scanLogLinesReverse(
  file: OpenLogFile,
  visit: (event: LogEvent) => boolean,
): Promise<void> {
  let end = file.size;
  let suffix = Buffer.alloc(0);
  let discardingOversized = false;
  while (end > 0) {
    const length = Math.min(LOG_READ_CHUNK_BYTES, end);
    const chunk = await readExact(file.handle, end - length, length);
    if (chunk.length === 0) break;
    end -= chunk.length;
    const combined = Buffer.concat([chunk, suffix]);
    let lineEnd = combined.length;
    for (let index = combined.length - 1; index >= 0; index -= 1) {
      if (combined[index] !== 0x0a) continue;
      const line = combined.subarray(index + 1, lineEnd);
      if (
        !discardingOversized &&
        line.length > 0 &&
        line.length <= MAX_EVENT_LINE_BYTES
      ) {
        const event = parseLogLines(`${line.toString("utf8")}\n`)[0];
        if (event && !visit(event)) return;
      }
      discardingOversized = false;
      lineEnd = index;
    }
    const prefix = combined.subarray(0, lineEnd);
    if (discardingOversized || prefix.length > MAX_EVENT_LINE_BYTES) {
      suffix = Buffer.alloc(0);
      discardingOversized = true;
    } else {
      suffix = Buffer.from(prefix);
    }
  }
  if (
    !discardingOversized &&
    suffix.length > 0 &&
    suffix.length <= MAX_EVENT_LINE_BYTES
  ) {
    const event = parseLogLines(`${suffix.toString("utf8")}\n`)[0];
    if (event) visit(event);
  }
}

/** Reads a finite validated snapshot without opening the LocalBase database. */
export async function readLogSnapshot(
  root: string,
  filters: LogFilters = {},
  limit = DEFAULT_LOG_LIMIT,
): Promise<LogEvent[]> {
  if (!Number.isInteger(limit) || limit < 1 || limit > MAX_LOG_LIMIT) {
    throw new Error(`Log limit must be between 1 and ${MAX_LOG_LIMIT}.`);
  }
  const canonical = canonicalLocalBaseRoot(root);
  assertInitializedLocalBaseRoot(canonical);
  const events: LogEvent[] = [];
  const files = await openLogGeneration(canonical);
  try {
    for (const file of files.reverse()) {
      await scanLogLinesReverse(file, (event) => {
        if (matchesLogFilters(event, filters)) events.push(event);
        return events.length < limit;
      });
      if (events.length >= limit) break;
    }
  } finally {
    await closeLogFiles(files);
  }
  return events.reverse();
}

type FollowFileState = {
  identity: string;
  offset: number;
  remainder: Buffer;
  handle: FileHandle;
  pathVisible: boolean;
  prefixLength: number;
  prefixDigest?: string;
};

async function digestLogPrefix(contents: Buffer): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    Uint8Array.from(contents),
  );
  return Buffer.from(digest).toString("hex");
}

async function followPrefixChanged(
  state: FollowFileState,
  targetSize: number,
): Promise<boolean> {
  if (state.prefixLength === 0 || state.prefixDigest === undefined)
    return false;
  if (targetSize < state.prefixLength) return true;
  const prefix = await readExact(state.handle, 0, state.prefixLength);
  return (
    prefix.length !== state.prefixLength ||
    (await digestLogPrefix(prefix)) !== state.prefixDigest
  );
}

async function refreshFollowPrefix(state: FollowFileState): Promise<void> {
  const size = (await state.handle.stat()).size;
  const prefixLength = Math.min(size, LOG_FOLLOW_FINGERPRINT_BYTES);
  if (prefixLength === 0) {
    state.prefixLength = 0;
    state.prefixDigest = undefined;
    return;
  }
  const prefix = await readExact(state.handle, 0, prefixLength);
  if (prefix.length !== prefixLength) return;
  state.prefixLength = prefixLength;
  state.prefixDigest = await digestLogPrefix(prefix);
}

async function waitForLogPoll(signal: AbortSignal): Promise<void> {
  if (signal.aborted) return;
  await new Promise<void>((resolve) => {
    const onAbort = () => complete();
    const timer = setTimeout(complete, LOG_FOLLOW_POLL_MS);
    function complete(): void {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      resolve();
    }
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

function parseCompleteLogLines(contents: Buffer): {
  events: LogEvent[];
  remainder: Buffer;
} {
  const lastNewline = contents.lastIndexOf(0x0a);
  if (lastNewline < 0) {
    return {
      events: [],
      remainder:
        contents.length <= MAX_EVENT_LINE_BYTES ? contents : Buffer.alloc(0),
    };
  }
  const complete = contents.subarray(0, lastNewline + 1).toString("utf8");
  return {
    events: parseLogLines(complete),
    remainder: contents.subarray(lastNewline + 1),
  };
}

export type FollowLogOptions = {
  pollMs?: number;
  onRead?: (identity: string, offset: number, length: number) => Promise<void>;
};

async function drainFollowState(
  state: FollowFileState,
  filters: LogFilters,
  emit: (event: LogEvent) => void | Promise<void>,
  onRead?: FollowLogOptions["onRead"],
): Promise<void> {
  const targetSize = (await state.handle.stat()).size;
  if (
    targetSize < state.offset ||
    (await followPrefixChanged(state, targetSize))
  ) {
    state.offset = 0;
    state.remainder = Buffer.alloc(0);
  }
  while (state.offset < targetSize) {
    const length = Math.min(LOG_READ_CHUNK_BYTES, targetSize - state.offset);
    const chunk = await readExact(state.handle, state.offset, length);
    if (chunk.length === 0) break;
    await onRead?.(state.identity, state.offset, chunk.length);
    state.offset += chunk.length;
    const combined = Buffer.concat([state.remainder, chunk]);
    const parsed = parseCompleteLogLines(combined);
    state.remainder = parsed.remainder;
    for (const event of parsed.events) {
      if (matchesLogFilters(event, filters)) await emit(event);
    }
  }
  await refreshFollowPrefix(state);
}

/** Follows stable file handles and drains rotated inodes before releasing them. */
export async function followLogEvents(
  root: string,
  filters: LogFilters,
  emit: (event: LogEvent) => void | Promise<void>,
  signal: AbortSignal,
  options: FollowLogOptions = {},
): Promise<void> {
  const canonical = canonicalLocalBaseRoot(root);
  assertInitializedLocalBaseRoot(canonical);
  const states = new Map<string, FollowFileState>();
  try {
    while (!signal.aborted) {
      for (const state of states.values()) state.pathVisible = false;
      for (const path of await orderedLogPaths(canonical)) {
        try {
          const opened = await openOwnedRegularFile(path, constants.O_RDONLY);
          const identity = fileIdentity(opened.stat);
          const existing = states.get(identity);
          if (existing) {
            existing.pathVisible = true;
            await opened.handle.close();
          } else {
            states.set(identity, {
              identity,
              offset: 0,
              remainder: Buffer.alloc(0),
              handle: opened.handle,
              pathVisible: true,
              prefixLength: 0,
            });
          }
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
        }
      }
      for (const [identity, state] of states) {
        await drainFollowState(state, filters, emit, options.onRead);
        const size = (await state.handle.stat()).size;
        if (!state.pathVisible && state.offset >= size) {
          await state.handle.close();
          states.delete(identity);
        }
      }
      if (signal.aborted) break;
      if (options.pollMs === undefined) await waitForLogPoll(signal);
      else {
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, options.pollMs);
          signal.addEventListener(
            "abort",
            () => {
              clearTimeout(timer);
              resolve();
            },
            { once: true },
          );
        });
      }
    }
  } finally {
    await Promise.all(
      [...states.values()].map((state) => state.handle.close().catch(() => {})),
    );
  }
}

export function formatHumanLogEvent(event: LogEvent): string {
  const trace = event.trace
    ? ` trace=${event.trace.traceId}/${event.trace.spanId}`
    : "";
  const request = event.http
    ? ` ${event.http.method} ${event.http.path} ${event.http.status} ${event.http.durationMs.toFixed(1)}ms`
    : "";
  return `${event.timestamp} ${event.severity.toUpperCase()} ${event.component} ${event.message}${trace}${request}`;
}
