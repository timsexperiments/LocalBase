import { z } from "zod";

// This module is shared with the browser bundle, so it imports nothing but zod.

/** Voice names the catalog runtimes provide; never usable as custom names. */
export const builtInSpeechVoices = [
  "default",
  "harbor",
  "willow",
  "af_heart",
  "af_bella",
] as const;

/**
 * Custom voice profiles are addressed by a server-issued id, never by a path,
 * URL or display name, so a request cannot point the runtime at other files.
 */
export const customSpeechVoiceIdSchema = z
  .string()
  .regex(/^voice-[0-9a-f]{12}$/, "must be a custom voice id");

/** Shortest reference clip; the speaker encoder needs a few seconds of speech. */
export const VOICE_MIN_SECONDS = 3;
/** Longest reference clip. The bundled references are 10 s. */
export const VOICE_MAX_SECONDS = 30;
/** Upload cap: 30 s of 48 kHz stereo 32-bit audio plus headers fits. */
export const VOICE_MAX_UPLOAD_BYTES = 12 * 1024 * 1024;
/** Each profile becomes a resident speaker reference in the warm runtime. */
export const CUSTOM_VOICE_LIMIT = 32;
/** Stored references are normalized to what the speaker encoder resamples to. */
export const VOICE_SAMPLE_RATE_HZ = 24_000;

export const VOICE_CONSENT_STATEMENT_ID = "voice-consent-v1";
export const VOICE_CONSENT_STATEMENT =
  "I have the right to use this voice: it is my own, or its speaker has given permission for it to be cloned here.";

/** Built-in voice names stay reserved, compared case-insensitively. */
export const reservedVoiceNames: readonly string[] = builtInSpeechVoices;

export function voiceNameKey(name: string): string {
  return name.normalize("NFKC").toLowerCase();
}

export const customVoiceNameSchema = z
  .string()
  .transform((name) => name.normalize("NFC").trim().replace(/\s+/g, " "))
  .pipe(
    z
      .string()
      .min(1, "Voice name is required.")
      .max(48, "Voice name must be at most 48 characters.")
      .refine(
        (name) => !/[\p{Cc}\p{Cf}]/u.test(name),
        "Voice name cannot contain control characters.",
      )
      .refine(
        (name) => !reservedVoiceNames.includes(voiceNameKey(name)),
        "That name belongs to a built-in voice.",
      )
      .refine(
        (name) => !customSpeechVoiceIdSchema.safeParse(name).success,
        "Voice name cannot look like a voice id.",
      ),
  );

export const customVoiceSchema = z
  .object({
    id: customSpeechVoiceIdSchema,
    name: z.string().min(1),
    durationMs: z.number().int().positive(),
    sizeBytes: z.number().int().positive(),
    sha256: z.string().regex(/^[a-f0-9]{64}$/),
    createdAt: z.iso.datetime({ offset: true }),
    consent: z
      .object({
        statement: z.literal(VOICE_CONSENT_STATEMENT_ID),
        attestedAt: z.iso.datetime({ offset: true }),
      })
      .strict(),
    /** Present only for callers that can manage voices. */
    createdBy: z.string().min(1).optional(),
  })
  .strict();
export type CustomVoice = z.infer<typeof customVoiceSchema>;

export const customVoiceListSchema = z
  .object({
    object: z.literal("list"),
    data: z.array(customVoiceSchema),
    canManage: z.boolean(),
    limits: z
      .object({
        minSeconds: z.number().positive(),
        maxSeconds: z.number().positive(),
        maxUploadBytes: z.number().int().positive(),
        maxVoices: z.number().int().positive(),
      })
      .strict(),
    consentStatement: z.string().min(1),
  })
  .strict();
export type CustomVoiceList = z.infer<typeof customVoiceListSchema>;

export const customVoiceRenameSchema = z
  .object({ name: customVoiceNameSchema })
  .strict();

export type VoiceErrorCode =
  | "voice_not_found"
  | "voice_name_taken"
  | "voice_limit_reached"
  | "consent_required"
  | "invalid_voice_name"
  | "unsupported_audio_format"
  | "audio_too_short"
  | "audio_too_long"
  | "audio_too_large"
  | "audio_silent";

/** A rejected voice operation; messages never quote the uploaded audio. */
export class VoiceError extends Error {
  constructor(
    readonly code: VoiceErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "VoiceError";
  }
}
