import { createHash, randomBytes } from "node:crypto";
import { chmod, lstat, mkdir, readFile, rename, rm } from "node:fs/promises";
import { join } from "node:path";
import { asc, count, eq } from "drizzle-orm";
import type { LocalBaseDatabase } from "../../db/client";
import { ttsCustomVoicesTable } from "../../db/schema";
import { canonicalLocalBaseRoot } from "../../utils/root";
import { normalizeVoiceAudio } from "./voice-audio";
import {
  CUSTOM_VOICE_LIMIT,
  customSpeechVoiceIdSchema,
  VOICE_CONSENT_STATEMENT_ID,
  VoiceError,
  customVoiceNameSchema,
  voiceNameKey,
  type CustomVoice,
} from "./voice-contract";

export type CustomVoiceRecord = CustomVoice & { createdBy: string };

export type CreateCustomVoiceInput = Readonly<{
  name: string;
  audio: Uint8Array;
  /** Must be literally true: the creator attests they may use this voice. */
  consent: boolean;
  createdBy: string;
}>;

export interface CustomVoiceStore {
  list(): CustomVoiceRecord[];
  get(id: string): CustomVoiceRecord | undefined;
  /** The reference path of a stored voice whose file still matches its checksum. */
  resolve(id: string): Promise<string | undefined>;
  /** Every stored voice whose file still matches its checksum, by id. */
  referenceFiles(): Promise<Readonly<Record<string, string>>>;
  create(input: CreateCustomVoiceInput): Promise<CustomVoiceRecord>;
  rename(id: string, name: string): CustomVoiceRecord;
  remove(id: string): Promise<CustomVoiceRecord>;
}

/** Private directory for reference clips; never served or listed directly. */
export function customVoicesDirectory(root: string): string {
  return join(canonicalLocalBaseRoot(root), "voices");
}

/** Builds a reference path from a validated id only, never from user text. */
export function customVoicePath(root: string, id: string): string {
  const voiceId = customSpeechVoiceIdSchema.parse(id);
  return join(customVoicesDirectory(root), `${voiceId}.wav`);
}

function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function fromRow(
  row: typeof ttsCustomVoicesTable.$inferSelect,
): CustomVoiceRecord {
  return {
    id: row.id,
    name: row.name,
    durationMs: row.durationMs,
    sizeBytes: row.sizeBytes,
    sha256: row.sha256,
    createdAt: row.createdAt,
    createdBy: row.createdBy,
    consent: {
      statement: VOICE_CONSENT_STATEMENT_ID,
      attestedAt: row.consentAttestedAt,
    },
  };
}

function parseName(name: string): string {
  const parsed = customVoiceNameSchema.safeParse(name);
  if (!parsed.success) {
    throw new VoiceError(
      "invalid_voice_name",
      parsed.error.issues[0]?.message ?? "Invalid voice name.",
    );
  }
  return parsed.data;
}

function notFound(): never {
  throw new VoiceError("voice_not_found", "Voice not found.");
}

/** Creates the owner-only voice directory and refuses a planted symlink. */
async function ensurePrivateDirectory(directory: string): Promise<void> {
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const info = await lstat(directory);
  if (!info.isDirectory()) {
    throw new Error("The custom voice directory is not a directory.");
  }
  if ((info.mode & 0o777) !== 0o700) await chmod(directory, 0o700);
}

function isUniqueViolation(error: unknown): boolean {
  return (
    error instanceof Error &&
    /UNIQUE constraint failed/i.test(
      `${error.message} ${String((error as { cause?: unknown }).cause ?? "")}`,
    )
  );
}

export function createCustomVoiceStore(
  database: LocalBaseDatabase,
  root: string,
): CustomVoiceStore {
  const table = ttsCustomVoicesTable;
  const directory = customVoicesDirectory(root);
  const row = (id: string) =>
    customSpeechVoiceIdSchema.safeParse(id).success
      ? database.select().from(table).where(eq(table.id, id)).get()
      : undefined;
  const assertNameAvailable = (name: string, except?: string) => {
    const existing = database
      .select({ id: table.id })
      .from(table)
      .where(eq(table.nameKey, voiceNameKey(name)))
      .get();
    if (existing && existing.id !== except) {
      throw new VoiceError(
        "voice_name_taken",
        "Another voice already uses that name.",
      );
    }
  };
  const fileMatches = async (record: CustomVoiceRecord) => {
    try {
      return sha256(await readFile(customVoicePath(root, record.id))) ===
        record.sha256
        ? customVoicePath(root, record.id)
        : undefined;
    } catch {
      return undefined;
    }
  };

  return {
    list() {
      return database
        .select()
        .from(table)
        .orderBy(asc(table.createdAt), asc(table.id))
        .all()
        .map(fromRow);
    },
    get(id) {
      const found = row(id);
      return found ? fromRow(found) : undefined;
    },
    async resolve(id) {
      const record = this.get(id);
      return record ? await fileMatches(record) : undefined;
    },
    async referenceFiles() {
      const entries = await Promise.all(
        this.list().map(
          async (record) => [record.id, await fileMatches(record)] as const,
        ),
      );
      return Object.freeze(
        Object.fromEntries(
          entries.filter(
            (entry): entry is readonly [string, string] =>
              entry[1] !== undefined,
          ),
        ),
      );
    },
    async create(input) {
      if (input.consent !== true) {
        throw new VoiceError(
          "consent_required",
          "Confirm that you have permission to use this voice.",
        );
      }
      const name = parseName(input.name);
      assertNameAvailable(name);
      const total =
        database.select({ value: count() }).from(table).get()?.value ?? 0;
      if (total >= CUSTOM_VOICE_LIMIT) {
        throw new VoiceError(
          "voice_limit_reached",
          `At most ${CUSTOM_VOICE_LIMIT} custom voices can be stored. Delete one first.`,
        );
      }
      const audio = normalizeVoiceAudio(input.audio);
      const id = customSpeechVoiceIdSchema.parse(
        `voice-${randomBytes(6).toString("hex")}`,
      );
      const now = new Date().toISOString();
      const record: CustomVoiceRecord = {
        id,
        name,
        durationMs: audio.durationMs,
        sizeBytes: audio.wav.byteLength,
        sha256: sha256(audio.wav),
        createdAt: now,
        createdBy: input.createdBy,
        consent: { statement: VOICE_CONSENT_STATEMENT_ID, attestedAt: now },
      };
      await ensurePrivateDirectory(directory);
      const path = customVoicePath(root, id);
      const temporary = `${path}.${randomBytes(4).toString("hex")}.partial`;
      try {
        await Bun.write(temporary, audio.wav, { mode: 0o600 });
        await chmod(temporary, 0o600);
        await rename(temporary, path);
        database
          .insert(table)
          .values({
            id,
            name,
            nameKey: voiceNameKey(name),
            sha256: record.sha256,
            sizeBytes: record.sizeBytes,
            durationMs: record.durationMs,
            createdAt: now,
            createdBy: input.createdBy,
            consentStatement: VOICE_CONSENT_STATEMENT_ID,
            consentAttestedAt: now,
          })
          .run();
      } catch (error) {
        await rm(temporary, { force: true });
        await rm(path, { force: true });
        if (isUniqueViolation(error)) {
          throw new VoiceError(
            "voice_name_taken",
            "Another voice already uses that name.",
          );
        }
        throw error;
      }
      return record;
    },
    rename(id, name) {
      const existing = row(id);
      if (!existing) notFound();
      const parsed = parseName(name);
      assertNameAvailable(parsed, id);
      try {
        database
          .update(table)
          .set({ name: parsed, nameKey: voiceNameKey(parsed) })
          .where(eq(table.id, id))
          .run();
      } catch (error) {
        if (isUniqueViolation(error)) {
          throw new VoiceError(
            "voice_name_taken",
            "Another voice already uses that name.",
          );
        }
        throw error;
      }
      return { ...fromRow(existing), name: parsed };
    },
    async remove(id) {
      const existing = row(id);
      if (!existing) notFound();
      // The row goes first, so a request racing the delete fails cleanly.
      database.delete(table).where(eq(table.id, id)).run();
      await rm(customVoicePath(root, id), { force: true });
      return fromRow(existing);
    },
  };
}
