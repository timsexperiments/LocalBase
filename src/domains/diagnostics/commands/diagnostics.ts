import { basename, extname, isAbsolute, join, resolve } from "node:path";
import { zipSync } from "fflate";
import { z } from "zod";
import {
  CATALOG,
  modelModalitySchema,
  primaryArtifact,
  resolveCatalogInstallation,
  type ModelKind,
} from "../../../catalog";
import {
  capabilityLabel,
  capabilityMismatchSchema,
  capabilitySchema,
  capabilitySourceSchema,
  unsupportedCapabilitySchema,
} from "../../../catalog/capabilities";
import {
  catalogDiagnostics,
  catalogProvenance,
  catalogTierSchema,
  catalogTrustSchema,
} from "../../../catalog/sources";
import { catalogFeedStatus } from "../../../catalog/remote-feed";
import { openReadOnlyDatabase } from "../../../db/client";
import type { CapabilityMismatch } from "../../models/capability-detection";
import { resolveCapabilitiesForModel } from "../../models/model-capabilities";
import { evaluateModelFit } from "../../models/memory-fit";
import {
  createObservedMemoryStore,
  type ObservedMemoryRecord,
} from "../../runtime/observed-memory";
import { directoryForModel } from "../../models/model-metadata";
import { createRuntimeCapabilityStore } from "../../runtime/runtime-capabilities";
import { detectSpecs, type HostSpecs } from "../../../system";
import { readConfig, type LocalBaseConfig } from "../../../manager";
import type { MinimalAppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { DiagnosticsInput } from "../../app/commands/inputs";
import { diagnosticsResultSchema } from "../../app/commands/results";
import {
  redactLogEventForDiagnostics,
  type DiagnosticsLogEvent,
  readLogSnapshot,
  type LogEvent,
} from "../../observability/logging";
import {
  resolveOtelConfiguration,
  type OtelConfiguration,
} from "../../observability/otel";
import {
  getServiceInspectionReadOnly,
  type ServiceInspection,
} from "../../service/manager";
import { LOCALBASE_VERSION } from "../../../version";
import {
  openSecureDirectory,
  outputParent,
} from "../../../utils/secure-file-publication";

const MAX_LOG_EVENTS = 500;
const MAX_LOG_BYTES = 2 * 1024 * 1024;
const MAX_UNCOMPRESSED_BYTES = 5 * 1024 * 1024;
const MAX_ARCHIVE_BYTES = 5 * 1024 * 1024;

const errorCodeSchema = z.enum([
  "hardware_unavailable",
  "configuration_unavailable",
  "models_unavailable",
  "service_unavailable",
  "otel_unavailable",
  "logs_unavailable",
]);

const availabilitySchema = <T extends z.ZodType>(data: T) =>
  z.discriminatedUnion("available", [
    z.object({ available: z.literal(true), data }).strict(),
    z.object({ available: z.literal(false), error: errorCodeSchema }).strict(),
  ]);

const hardwareSchema = z
  .object({
    os: z.string().min(1),
    arch: z.string().min(1),
    cpu: z.string().min(1),
    gpu: z.string().min(1),
    memoryGb: z.number().finite().nonnegative(),
    vramGb: z.number().finite().nonnegative(),
  })
  .strict();

const configurationSchema = z
  .object({
    contextSize: z.number().int().positive(),
    contextMode: z.enum(["auto", "explicit"]),
    parallel: z.union([z.literal("auto"), z.number().int().min(1).max(4)]),
    selectedModels: z
      .object({
        llm: z.array(z.string().min(1)),
        stt: z.array(z.string().min(1)),
        tts: z.array(z.string().min(1)),
        image: z.array(z.string().min(1)),
        video: z.array(z.string().min(1)),
      })
      .strict(),
    activeModels: z
      .object({
        llm: z.string().min(1),
        stt: z.string(),
        tts: z.string(),
        image: z.string(),
        video: z.string(),
      })
      .strict(),
  })
  .strict();

const modelSchema = z
  .object({
    id: z.string().min(1),
    kind: z.enum(["llm", "stt", "tts", "image", "video"]),
    size: z.string().min(1),
    storageGb: z.number().positive(),
    /** How far LocalBase trusts the catalog entry, and which tier it came from. */
    trust: catalogTrustSchema,
    catalogTier: catalogTierSchema,
    /** Top-level fields a higher catalog tier changed. */
    patchedFields: z.array(z.string().min(1)),
    unsupportedCapabilities: z.array(unsupportedCapabilitySchema),
    selected: z.boolean(),
    active: z.boolean(),
    complete: z.boolean(),
    capabilities: z.array(
      z
        .object({ id: capabilitySchema, source: capabilitySourceSchema })
        .strict(),
    ),
    capabilityMismatches: z.array(capabilityMismatchSchema),
    inputModalities: z
      .object({
        values: z.array(modelModalitySchema).min(1),
        source: capabilitySourceSchema,
      })
      .strict(),
    capabilityEvidence: z.array(z.string().min(1)),
    runtimeBuild: z.string().min(1).nullable(),
    memory: z
      .object({
        minimumGb: z.number().int().positive(),
        source: z.enum(["estimate", "measured", "observed"]),
        launchShape: z.string().min(1),
        observedPeakBytes: z.number().int().positive().nullable(),
        verdict: z.enum(["fits", "tight", "wont-fit"]),
        systemMemoryGb: z.number().nonnegative(),
      })
      .strict(),
  })
  .strict();

const serviceSchema = z
  .object({
    manager: z.enum(["launchd", "systemd-user"]),
    state: z.string().min(1),
    enabled: z.boolean().nullable(),
    definitionInstalled: z.boolean(),
    managerAvailable: z.boolean(),
    managerState: z
      .string()
      .min(1)
      .max(128)
      .regex(/^[A-Za-z0-9._/-]+$/)
      .nullable(),
    pid: z.number().int().positive().nullable(),
    uptimeSeconds: z.number().int().nonnegative().nullable(),
    restartCount: z.number().int().nonnegative().nullable(),
    gateway: z
      .object({
        state: z.enum(["ready", "not_ready"]),
        health: z.unknown().optional(),
      })
      .strict(),
  })
  .strict();

const observabilitySchema = z
  .object({
    enabled: z.boolean(),
    source: z.enum(["disabled", "persistent", "environment"]),
    endpoint: z.string(),
    sampleRatio: z.number().min(0).max(1),
  })
  .strict();

const logsSchema = z
  .object({
    includedEvents: z.number().int().nonnegative(),
    window: z
      .object({
        oldest: z.string().optional(),
        newest: z.string().optional(),
      })
      .strict(),
    bytes: z.number().int().nonnegative(),
  })
  .strict();

export const diagnosticsManifestSchema = z
  .object({
    schemaVersion: z.literal(1),
    generatedAt: z.iso.datetime({ offset: true }),
    version: z.string().min(1),
    platform: z
      .object({ os: z.string().min(1), arch: z.string().min(1) })
      .strict(),
    hardware: availabilitySchema(hardwareSchema),
    configuration: availabilitySchema(configurationSchema),
    models: availabilitySchema(
      z
        .object({
          models: z.array(modelSchema),
          /** Catalog files that were skipped, by file name, with the reason. */
          catalogIssues: z.array(
            z
              .object({
                tier: catalogTierSchema,
                file: z.string().min(1),
                modelId: z.string().min(1).optional(),
                message: z.string().min(1),
              })
              .strict(),
          ),
          /** The remote community catalog feed and its last download. */
          catalogFeed: z
            .object({
              enabled: z.boolean(),
              /** Without query string or fragment. */
              url: z.string().min(1).nullable(),
              source: z.enum(["default", "settings", "environment"]),
              problem: z.string().min(1).nullable(),
              fetchedAt: z.string().nullable(),
              checkedAt: z.string().nullable(),
              revision: z.string().nullable(),
              stale: z.boolean(),
              lastError: z.string().min(1).nullable(),
              entries: z.number().int().nonnegative(),
              builtIn: z.number().int().nonnegative(),
              added: z.array(z.string().min(1)),
              skipped: z.number().int().nonnegative(),
            })
            .strict(),
        })
        .strict(),
    ),
    service: availabilitySchema(serviceSchema),
    observability: availabilitySchema(observabilitySchema),
    logs: availabilitySchema(logsSchema),
  })
  .strict();

type DiagnosticsManifest = z.infer<typeof diagnosticsManifestSchema>;

type ArchiveEntry = { name: string; data: Uint8Array };

function unavailable(code: z.infer<typeof errorCodeSchema>) {
  return { available: false as const, error: code };
}

function hardwareData(specs: HostSpecs) {
  return hardwareSchema.parse({
    os: specs.osName,
    arch: process.arch,
    cpu: specs.cpuModel,
    gpu: specs.gpuName,
    memoryGb: specs.ramGb,
    vramGb: specs.gpuVramGb,
  });
}

function configurationData(config: LocalBaseConfig) {
  return configurationSchema.parse({
    contextSize: config.ctxSize,
    contextMode: config.ctxMode,
    parallel: config.parallel,
    selectedModels: {
      llm: config.selectedLlmModels,
      stt: config.selectedSttModels,
      tts: config.selectedTtsModels,
      image: config.selectedImageModels,
      video: config.selectedVideoModels,
    },
    activeModels: {
      llm: config.activeLlmModel,
      stt: config.activeSttModel,
      tts: config.activeTtsModel,
      image: config.activeImageModel,
      video: config.activeVideoModel,
    },
  });
}

async function modelData(
  config: LocalBaseConfig,
  root: string,
  systemMemoryGb: number,
) {
  const selected = new Set([
    ...config.selectedLlmModels,
    ...config.selectedSttModels,
    ...config.selectedTtsModels,
    ...config.selectedImageModels,
    ...config.selectedVideoModels,
  ]);
  const active = new Set([
    config.activeLlmModel,
    config.activeSttModel,
    config.activeTtsModel,
    config.activeImageModel,
    config.activeVideoModel,
  ]);
  const selectedByKind: Record<ModelKind, string[]> = {
    llm: config.selectedLlmModels,
    stt: config.selectedSttModels,
    tts: config.selectedTtsModels,
    image: config.selectedImageModels,
    video: config.selectedVideoModels,
  };
  const models = [];
  const runtimeStore = openRuntimeCapabilityStore(root);
  try {
    for (const model of CATALOG) {
      const installation = await resolveCatalogInstallation(
        model,
        directoryForModel(config, model.kind),
      );
      if (!selected.has(model.modelId) && !installation.complete) continue;
      const sha256 = primaryArtifact(model).sha256;
      let observed: ObservedMemoryRecord[] = [];
      if (runtimeStore && sha256) {
        try {
          observed = runtimeStore.observed.list(model.modelId, sha256);
        } catch {
          // A database from before migration 0013 has no observed peaks.
        }
      }
      const fit = evaluateModelFit(model, systemMemoryGb, { observed });
      const report = await resolveCapabilitiesForModel(
        model,
        config,
        runtimeStore?.store ?? null,
      );
      const provenance = catalogProvenance(model.modelId);
      models.push(
        modelSchema.parse({
          id: model.modelId,
          kind: model.kind,
          size: model.size,
          storageGb: model.storageGb,
          trust: provenance.trust,
          catalogTier: provenance.tier,
          patchedFields: [...provenance.patchedFields],
          unsupportedCapabilities: model.unsupportedCapabilities ?? [],
          selected: selectedByKind[model.kind].includes(model.modelId),
          active: active.has(model.modelId),
          complete: installation.complete,
          capabilities: report.capabilities.map(({ id, source }) => ({
            id,
            source,
          })),
          capabilityMismatches: report.mismatches,
          inputModalities: report.inputModalities,
          capabilityEvidence: report.evidence,
          runtimeBuild: report.runtimeBuild,
          memory: {
            minimumGb: fit.minVramGb,
            source: fit.source,
            launchShape: fit.figure.launchShape,
            observedPeakBytes: fit.figure.observed?.peakBytes ?? null,
            verdict: fit.verdict,
            systemMemoryGb,
          },
        }),
      );
    }
  } finally {
    runtimeStore?.close();
  }
  const catalogIssues = catalogDiagnostics().map((issue) => ({
    tier: issue.tier,
    file: basename(issue.path),
    ...(issue.modelId ? { modelId: issue.modelId } : {}),
    message: issue.message.split(root).join("<root>"),
  }));
  const redact = (text: string | null) =>
    text === null ? null : text.split(root).join("<root>");
  const feed = catalogFeedStatus(root);
  const catalogFeed = {
    ...feed,
    url: feed.url ? feedUrlForDiagnostics(feed.url) : null,
    problem: redact(feed.problem),
    lastError: redact(feed.lastError),
    added: [...feed.added],
  };
  return { models, catalogIssues, catalogFeed };
}

/** Drops credentials-bearing parts (query, fragment) from a feed URL. */
function feedUrlForDiagnostics(url: string): string {
  try {
    const parsed = new URL(url);
    parsed.search = "";
    parsed.hash = "";
    parsed.username = "";
    parsed.password = "";
    return parsed.toString();
  } catch {
    return "<invalid>";
  }
}

/**
 * Opens the runtime capability table read-only. Diagnostics must never create
 * or migrate a database, so a missing or older database yields no store.
 */
function openRuntimeCapabilityStore(root: string) {
  try {
    const database = openReadOnlyDatabase(root);
    return {
      store: createRuntimeCapabilityStore(database.db),
      observed: createObservedMemoryStore(database.db),
      close: database.close,
    };
  } catch {
    return null;
  }
}

function formatMismatch(mismatch: CapabilityMismatch): string {
  if (mismatch.unsupportedReason !== undefined) {
    return `${mismatch.id} (verified unsupported: ${mismatch.unsupportedReason}; using ${mismatch.effective})`;
  }
  const sources = [
    `declared ${mismatch.declared}`,
    ...(mismatch.detected === undefined
      ? []
      : [`detected ${mismatch.detected}`]),
    ...(mismatch.runtime === undefined ? [] : [`runtime ${mismatch.runtime}`]),
    ...(mismatch.verified === undefined
      ? []
      : [`round-trip ${mismatch.verified}`]),
  ];
  return `${mismatch.id} (${sources.join(", ")}; using ${mismatch.effective})`;
}

function serviceData(inspection: ServiceInspection) {
  return serviceSchema.parse({
    manager: inspection.service.manager,
    state: inspection.service.state,
    enabled: inspection.service.enabled,
    definitionInstalled: inspection.service.definitionInstalled,
    managerAvailable: inspection.service.managerAvailable,
    managerState: inspection.service.managerState,
    pid: inspection.service.pid,
    uptimeSeconds: inspection.service.uptimeSeconds,
    restartCount: inspection.service.restartCount,
    gateway: {
      state: inspection.gateway.state,
      ...(inspection.gateway.health
        ? { health: inspection.gateway.health }
        : {}),
    },
  });
}

function observabilityData(
  config: LocalBaseConfig,
): z.infer<typeof observabilitySchema> {
  const otel: OtelConfiguration = resolveOtelConfiguration(config, process.env);
  return observabilitySchema.parse({
    enabled: otel.enabled,
    source: otel.enabled ? otel.source : "disabled",
    endpoint: otel.enabled ? otel.displayEndpoint : "",
    sampleRatio: otel.sampleRatio,
  });
}

function boundedLogContents(events: LogEvent[]): {
  events: DiagnosticsLogEvent[];
  bytes: number;
} {
  const chosen: DiagnosticsLogEvent[] = [];
  let bytes = 0;
  for (const event of [...events].reverse()) {
    const sanitized = redactLogEventForDiagnostics(event);
    const line = `${JSON.stringify(sanitized)}\n`;
    const lineBytes = new TextEncoder().encode(line).byteLength;
    if (chosen.length >= MAX_LOG_EVENTS || bytes + lineBytes > MAX_LOG_BYTES)
      break;
    chosen.unshift(sanitized);
    bytes += lineBytes;
  }
  return { events: chosen, bytes };
}

function outputPath(requested?: string): string {
  if (requested && /[\u0000\r\n]/.test(requested)) {
    throw new Error("Diagnostics output path contains invalid characters.");
  }
  const value = requested
    ? resolve(process.cwd(), requested)
    : join(
        process.cwd(),
        `localbase-diagnostics-${Date.now()}-${crypto.randomUUID()}.zip`,
      );
  if (extname(value).toLowerCase() !== ".zip") {
    throw new Error("Diagnostics output must end in .zip.");
  }
  if (!isAbsolute(value) || value === "/") {
    throw new Error("Diagnostics output path is invalid.");
  }
  return value;
}

async function writeArchive(
  path: string,
  entries: ArchiveEntry[],
): Promise<number> {
  const uncompressed = entries.reduce(
    (sum, entry) => sum + entry.data.byteLength,
    0,
  );
  if (uncompressed > MAX_UNCOMPRESSED_BYTES) {
    throw new Error("Diagnostics archive exceeds the uncompressed size limit.");
  }
  const archive = zipSync(
    Object.fromEntries(entries.map((entry) => [entry.name, entry.data])),
    { level: 6 },
  );
  if (archive.byteLength > MAX_ARCHIVE_BYTES) {
    throw new Error("Diagnostics archive exceeds the size limit.");
  }
  const directory = openSecureDirectory(outputParent(path));
  const temporary = `.localbase-diagnostics-${crypto.randomUUID()}.tmp`;
  let file: number | undefined;
  try {
    file = directory.createExclusiveFile(temporary, 0o600);
    directory.write(file, archive);
    directory.sync(file);
    directory.closeFile(file);
    file = undefined;
    directory.publish(temporary, basename(path));
    directory.remove(temporary);
    directory.syncDirectory();
    return archive.byteLength;
  } finally {
    if (file !== undefined) directory.closeFile(file);
    directory.remove(temporary);
    directory.close();
  }
}

function section<T>(value: T): { available: true; data: T } {
  return { available: true, data: value };
}

export async function runDiagnostics(
  input: DiagnosticsInput,
  ctx: MinimalAppContext,
  execution: CommandExecution,
) {
  const root = ctx.config.root;
  const generatedAt = new Date().toISOString();
  const platform = { os: process.platform, arch: process.arch };

  let hardwareMemoryGb = 0;
  const hardware = await detectSpecs()
    .then((specs) => {
      hardwareMemoryGb = specs.gpuVramGb;
      return section(hardwareData(specs));
    })
    .catch(() => unavailable("hardware_unavailable"));

  let config: LocalBaseConfig | undefined;
  const configuration = await readConfig(root)
    .then((value) => {
      config = value;
      return section(configurationData(value));
    })
    .catch(() => unavailable("configuration_unavailable"));

  const models = config
    ? await modelData(config, root, hardwareMemoryGb)
        .then((value) => section(value))
        .catch(() => unavailable("models_unavailable"))
    : unavailable("models_unavailable");

  const service = await getServiceInspectionReadOnly(root)
    .then((value) => section(serviceData(value)))
    .catch(() => unavailable("service_unavailable"));

  const observability = config
    ? Promise.resolve()
        .then(() => section(observabilityData(config!)))
        .catch(() => unavailable("otel_unavailable"))
    : Promise.resolve(unavailable("otel_unavailable"));

  let capturedEvents: DiagnosticsLogEvent[] = [];
  const logResult = await readLogSnapshot(root, {}, MAX_LOG_EVENTS)
    .then((events) => {
      const bounded = boundedLogContents(events);
      capturedEvents = bounded.events;
      return section({
        includedEvents: bounded.events.length,
        window: {
          ...(bounded.events[0] ? { oldest: bounded.events[0].timestamp } : {}),
          ...(bounded.events.at(-1)
            ? { newest: bounded.events.at(-1)!.timestamp }
            : {}),
        },
        bytes: bounded.bytes,
      });
    })
    .catch(() => unavailable("logs_unavailable"));
  const logs = await logResult;

  const manifest: DiagnosticsManifest = diagnosticsManifestSchema.parse({
    schemaVersion: 1,
    generatedAt,
    version: LOCALBASE_VERSION,
    platform,
    hardware,
    configuration,
    models,
    service,
    observability: await observability,
    logs,
  });

  const logText = capturedEvents
    .map((event) => `${JSON.stringify(event)}\n`)
    .join("");
  const entries: ArchiveEntry[] = [
    {
      name: "manifest.json",
      data: new TextEncoder().encode(`${JSON.stringify(manifest, null, 2)}\n`),
    },
    { name: "logs/events.jsonl", data: new TextEncoder().encode(logText) },
  ];
  const path = outputPath(input.output);
  const bytes = await writeArchive(path, entries);
  const result = diagnosticsResultSchema.parse({
    archive: { path, entries: entries.length, bytes },
  });
  execution.output.info(`Created diagnostics bundle: ${path}`);
  if (manifest.models.available) {
    const feed = manifest.models.data.catalogFeed;
    execution.output.info(
      feed.enabled
        ? `Catalog feed: ${feed.url} (${feed.added.length} community model(s), ${feed.fetchedAt ? `fetched ${feed.fetchedAt}` : "never downloaded"}${feed.stale ? ", stale" : ""})${feed.lastError ? `; last error: ${feed.lastError}` : ""}`
        : "Catalog feed: off",
    );
    for (const issue of manifest.models.data.catalogIssues) {
      execution.output.info(
        `Catalog file skipped (${issue.tier}): ${issue.file}: ${issue.message}`,
      );
    }
    for (const model of manifest.models.data.models) {
      execution.output.info(
        `Memory for ${model.id}: ${model.memory.minimumGb} GB (${model.memory.source}), ${model.memory.verdict}`,
      );
      if (model.trust !== "qualified") {
        execution.output.info(
          `Catalog trust for ${model.id}: ${model.trust} (${model.catalogTier} tier${model.patchedFields.length > 0 ? `; changed ${model.patchedFields.join(", ")}` : ""})`,
        );
      }
      for (const unsupported of model.unsupportedCapabilities) {
        execution.output.info(
          `Verified unsupported for ${model.id}: ${capabilityLabel(unsupported.id)} (${unsupported.reason})`,
        );
      }
      if (model.capabilityMismatches.length === 0) continue;
      execution.output.info(
        `Capability mismatch for ${model.id}: ${model.capabilityMismatches
          .map(formatMismatch)
          .join("; ")}`,
      );
    }
  }
  return { data: result };
}
