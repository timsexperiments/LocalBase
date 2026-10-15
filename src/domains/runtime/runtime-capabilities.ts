import { and, eq } from "drizzle-orm";
import { z } from "zod";
import { modelModalitySchema, type ModelModality } from "../../catalog";
import { capabilitySchema, type Capability } from "../../catalog/capabilities";
import type { LocalBaseDatabase } from "../../db/client";
import { modelRuntimeCapabilitiesTable } from "../../db/schema";
import {
  analyzeChatTemplate,
  type CapabilityEvidence,
  type CapabilityOpinions,
} from "../models/capability-detection";

/** Capabilities a running backend reported, keyed to the exact artifact. */
export interface RuntimeCapabilityRecord extends CapabilityEvidence {
  readonly modelId: string;
  readonly artifactSha256: string;
  readonly inputModalities: readonly ModelModality[];
  readonly buildInfo: string;
  readonly observedAt: string;
}

const llamaPropsSchema = z
  .object({
    build_info: z.string().optional(),
    chat_template: z.string().optional(),
    modalities: z
      .object({
        vision: z.boolean().optional(),
        audio: z.boolean().optional(),
      })
      .partial()
      .optional(),
    chat_template_caps: z
      .object({ supports_tool_calls: z.boolean().optional() })
      .partial()
      .optional(),
  })
  .passthrough();

/**
 * Reads what llama-server reports in `/props`. Tool support comes from the
 * server's own template probe. The server has no reasoning flag, so the
 * template it actually loaded is checked for thinking markers.
 */
export function parseLlamaServerProps(
  body: unknown,
  options: Readonly<{ embedding: boolean }>,
): Omit<
  RuntimeCapabilityRecord,
  "modelId" | "artifactSha256" | "observedAt"
> | null {
  const parsed = llamaPropsSchema.safeParse(body);
  if (!parsed.success) return null;
  const props = parsed.data;
  const capabilities: Partial<Record<Capability, boolean>> = {};
  if (!options.embedding) {
    const toolCalls = props.chat_template_caps?.supports_tool_calls;
    if (toolCalls !== undefined) capabilities["tool-calling"] = toolCalls;
    if (
      props.chat_template &&
      analyzeChatTemplate(props.chat_template).reasoning
    )
      capabilities.reasoning = true;
  }
  const inputModalities: ModelModality[] = ["text"];
  if (props.modalities?.vision) inputModalities.push("image");
  if (props.modalities?.audio) inputModalities.push("audio");
  return {
    capabilities,
    inputModalities,
    buildInfo: props.build_info ?? "unknown",
  };
}

const storedOpinionsSchema = z.record(z.string(), z.boolean());
const storedModalitiesSchema = z.array(modelModalitySchema);

function decodeOpinions(text: string): CapabilityOpinions {
  try {
    const parsed = storedOpinionsSchema.parse(JSON.parse(text));
    return Object.fromEntries(
      Object.entries(parsed).filter(
        ([id]) => capabilitySchema.safeParse(id).success,
      ),
    );
  } catch {
    return {};
  }
}

function decodeModalities(text: string): ModelModality[] {
  try {
    return storedModalitiesSchema.parse(JSON.parse(text));
  } catch {
    return ["text"];
  }
}

export interface RuntimeCapabilityStore {
  record(record: RuntimeCapabilityRecord): void;
  read(modelId: string, artifactSha256: string): RuntimeCapabilityRecord | null;
}

/**
 * Persists runtime reports so they survive restarts. A new artifact hash is
 * a different row, so an updated model never inherits stale evidence.
 */
export function createRuntimeCapabilityStore(
  database: LocalBaseDatabase,
): RuntimeCapabilityStore {
  return {
    record(record) {
      const values = {
        modelId: record.modelId,
        artifactSha256: record.artifactSha256,
        capabilities: JSON.stringify(record.capabilities),
        inputModalities: JSON.stringify(record.inputModalities),
        buildInfo: record.buildInfo,
        observedAt: record.observedAt,
      };
      database
        .insert(modelRuntimeCapabilitiesTable)
        .values(values)
        .onConflictDoUpdate({
          target: [
            modelRuntimeCapabilitiesTable.modelId,
            modelRuntimeCapabilitiesTable.artifactSha256,
          ],
          set: {
            capabilities: values.capabilities,
            inputModalities: values.inputModalities,
            buildInfo: values.buildInfo,
            observedAt: values.observedAt,
          },
        })
        .run();
    },
    read(modelId, artifactSha256) {
      const row = database
        .select()
        .from(modelRuntimeCapabilitiesTable)
        .where(
          and(
            eq(modelRuntimeCapabilitiesTable.modelId, modelId),
            eq(modelRuntimeCapabilitiesTable.artifactSha256, artifactSha256),
          ),
        )
        .get();
      if (!row) return null;
      return {
        modelId: row.modelId,
        artifactSha256: row.artifactSha256,
        capabilities: decodeOpinions(row.capabilities),
        inputModalities: decodeModalities(row.inputModalities),
        buildInfo: row.buildInfo,
        observedAt: row.observedAt,
      };
    },
  };
}

export type LlamaServerProbe = Readonly<{
  baseUrl: string;
  modelId: string;
  artifactSha256: string;
  embedding: boolean;
  store: RuntimeCapabilityStore;
  fetch?: (url: string, init: RequestInit) => Promise<Response>;
  now?: () => Date;
  timeoutMs?: number;
}>;

/**
 * Asks a healthy llama-server what it supports and stores the answer.
 * Returns the stored record, or null when the server gave no usable answer.
 */
export async function probeLlamaServerCapabilities(
  probe: LlamaServerProbe,
): Promise<RuntimeCapabilityRecord | null> {
  const request = probe.fetch ?? fetch;
  const response = await request(`${probe.baseUrl}/props`, {
    signal: AbortSignal.timeout(probe.timeoutMs ?? 5_000),
  });
  if (!response.ok) return null;
  const parsed = parseLlamaServerProps(await response.json(), {
    embedding: probe.embedding,
  });
  if (!parsed) return null;
  const record: RuntimeCapabilityRecord = {
    ...parsed,
    modelId: probe.modelId,
    artifactSha256: probe.artifactSha256,
    observedAt: (probe.now?.() ?? new Date()).toISOString(),
  };
  probe.store.record(record);
  return record;
}
