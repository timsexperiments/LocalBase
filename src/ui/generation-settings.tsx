import type { ReactNode } from "react";
import { z } from "zod";
import type { Model, Mode } from "./client";

const imageSizeSchema = z.enum(["256x256", "512x512", "1024x1024"]);
export type GenerationSettings = {
  llm: { temperature?: number; max_tokens?: number };
  image: { size?: z.infer<typeof imageSizeSchema> };
  video: { negative_prompt?: string };
  tts: { voice?: string };
  stt: { language?: string; prompt?: string };
  embedding: { dimensions?: number };
};
export function defaultGenerationSettings(): GenerationSettings {
  return { llm: {}, image: {}, video: {}, tts: {}, stt: {}, embedding: {} };
}
export type GenerationPreferences = Record<string, GenerationSettings>;
export function modelGenerationSettings(
  preferences: GenerationPreferences,
  modelId: string | undefined,
): GenerationSettings {
  return modelId !== undefined && Object.hasOwn(preferences, modelId)
    ? preferences[modelId]
    : defaultGenerationSettings();
}

export function chatParameters(settings: GenerationSettings["llm"]) {
  return z
    .object({
      temperature: z.number().min(0).max(2).optional(),
      max_tokens: z.number().int().positive().optional(),
    })
    .parse(settings);
}
export function imageParameters(settings: GenerationSettings["image"]) {
  return z.object({ size: imageSizeSchema.optional() }).parse(settings);
}
export function videoParameters(settings: GenerationSettings["video"]) {
  return z
    .object({ negative_prompt: z.string().max(16384).optional() })
    .parse(settings);
}
export function speechParameters(
  model: Model,
  settings: GenerationSettings["tts"],
) {
  const cap = model.catalog.capabilities;
  if (cap?.kind !== "speech")
    throw new Error("Speech voice metadata is unavailable.");
  const voice = settings.voice ?? cap.voice.defaultRequestValue;
  if (!cap.voice.requestValues.some((value) => value === voice))
    throw new Error(
      `Voice ${voice} is not available for ${model.id}. Reset generation settings or choose an advertised voice.`,
    );
  return { voice };
}
export function embeddingParameters(
  model: Model,
  settings: GenerationSettings["embedding"],
) {
  const cap = model.catalog.capabilities;
  if (
    cap?.kind !== "embedding" ||
    settings.dimensions === undefined ||
    cap.dimensions.minimum === cap.dimensions.maximum
  )
    return {};
  return z
    .object({
      dimensions: z
        .number()
        .int()
        .min(cap.dimensions.minimum)
        .max(cap.dimensions.maximum),
    })
    .parse(settings);
}
export function transcriptionParameters(
  model: Model,
  settings: GenerationSettings["stt"],
) {
  // Catalog IDs identify English-only Whisper variants; multilingual flags are not exhaustive.
  const language = model.id.includes("-en-")
    ? undefined
    : settings.language?.trim();
  return {
    ...(language ? { language } : {}),
    ...(settings.prompt?.trim() ? { prompt: settings.prompt } : {}),
  };
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="generation-settings-field">
      <span>{label}</span>
      {children}
    </label>
  );
}
function NumberField({
  label,
  value,
  min,
  max,
  step = 1,
  onChange,
}: {
  label: string;
  value: number | undefined;
  min: number;
  max?: number;
  step?: number;
  onChange: (value: number | undefined) => void;
}) {
  return (
    <Field label={label}>
      <input
        className="generation-settings-control"
        type="number"
        min={min}
        max={max}
        step={step}
        value={value ?? ""}
        placeholder="Model default"
        onChange={(event) => {
          if (!event.currentTarget.value) onChange(undefined);
          else if (Number.isFinite(event.currentTarget.valueAsNumber))
            onChange(event.currentTarget.valueAsNumber);
        }}
        onBlur={(event) => {
          if (!event.currentTarget.value) return;
          const number = event.currentTarget.valueAsNumber;
          if (Number.isFinite(number))
            onChange(
              Math.min(
                max ?? Number.MAX_SAFE_INTEGER,
                Math.max(min, step === 1 ? Math.round(number) : number),
              ),
            );
        }}
      />
    </Field>
  );
}

export function GenerationSettingsFields({
  mode,
  model,
  settings,
  onChange,
}: {
  mode: Mode;
  model: Model;
  settings: GenerationSettings;
  onChange: (settings: GenerationSettings) => void;
}) {
  const cap = model.catalog.capabilities;
  const voices = cap?.kind === "speech" ? cap.voice.requestValues : [];
  return (
    <section className="generation-settings-section">
      {mode === "llm" && (
        <>
          <NumberField
            label="Temperature"
            value={settings.llm.temperature}
            min={0}
            max={2}
            step={0.1}
            onChange={(temperature) =>
              onChange({ ...settings, llm: { ...settings.llm, temperature } })
            }
          />
          <NumberField
            label="Maximum output tokens"
            value={settings.llm.max_tokens}
            min={1}
            onChange={(max_tokens) =>
              onChange({ ...settings, llm: { ...settings.llm, max_tokens } })
            }
          />
          {model?.catalog.contextWindowTokens && (
            <p className="generation-settings-hint">
              Catalog context window:{" "}
              {model.catalog.contextWindowTokens.toLocaleString()} tokens,
              shared by input and output. Runtime limits may be lower.
            </p>
          )}
        </>
      )}
      {mode === "image" && (
        <Field label="Image size">
          <select
            className="generation-settings-control"
            value={settings.image.size ?? ""}
            onChange={(event) =>
              onChange({
                ...settings,
                image: {
                  size: event.target.value
                    ? imageSizeSchema.parse(event.target.value)
                    : undefined,
                },
              })
            }
          >
            <option value="">Model default</option>
            {imageSizeSchema.options.map((size) => (
              <option key={size}>{size}</option>
            ))}
          </select>
        </Field>
      )}
      {mode === "video" && (
        <>
          {cap?.kind === "video" && (
            <p className="generation-settings-hint">
              {cap.width} × {cap.height}, {cap.frames} frames at {cap.fps} fps,{" "}
              {(cap.frames / cap.fps).toFixed(2)} seconds. Size, duration and
              seed are fixed by the qualified profile.
            </p>
          )}
          {cap?.kind === "video" && cap.mode === "t2v" && (
            <Field label="Negative prompt">
              <textarea
                className="generation-settings-control"
                maxLength={16384}
                value={settings.video.negative_prompt ?? ""}
                placeholder="Model default"
                onChange={(event) =>
                  onChange({
                    ...settings,
                    video: { negative_prompt: event.target.value || undefined },
                  })
                }
              />
            </Field>
          )}
        </>
      )}
      {mode === "tts" && (
        <>
          {settings.tts.voice &&
            !voices.some((voice) => voice === settings.tts.voice) && (
              <p className="generation-settings-hint" role="alert">
                Selected voice {settings.tts.voice} is not available for this
                model. Reset to model defaults before generating.
              </p>
            )}
          {voices.length > 1 ? (
            <Field label="Voice">
              <select
                className="generation-settings-control"
                value={settings.tts.voice ?? ""}
                onChange={(event) =>
                  onChange({
                    ...settings,
                    tts: { voice: event.target.value || undefined },
                  })
                }
              >
                <option value="">Model default</option>
                {voices.map((voice) => (
                  <option key={voice}>{voice}</option>
                ))}
              </select>
            </Field>
          ) : (
            <p className="generation-settings-hint">
              Only the default voice is available.
            </p>
          )}
          <p className="generation-settings-hint">
            WAV output. Speed is fixed at 1.
          </p>
        </>
      )}
      {mode === "stt" && (
        <>
          {model?.id.includes("-en-") ? (
            <p className="generation-settings-hint">
              This model transcribes English only.
            </p>
          ) : (
            <Field label="Language">
              <input
                className="generation-settings-control"
                value={settings.stt.language ?? ""}
                placeholder="Model default; e.g. en, es, auto"
                onChange={(event) =>
                  onChange({
                    ...settings,
                    stt: {
                      ...settings.stt,
                      language: event.target.value || undefined,
                    },
                  })
                }
              />
            </Field>
          )}
          <Field label="Transcription prompt">
            <textarea
              className="generation-settings-control"
              value={settings.stt.prompt ?? ""}
              placeholder="Optional vocabulary or context"
              onChange={(event) =>
                onChange({
                  ...settings,
                  stt: {
                    ...settings.stt,
                    prompt: event.target.value || undefined,
                  },
                })
              }
            />
          </Field>
        </>
      )}
      {mode === "embedding" &&
        cap?.kind === "embedding" &&
        (cap.dimensions.minimum === cap.dimensions.maximum ? (
          <p className="generation-settings-hint">
            Fixed output: {cap.dimensions.maximum} dimensions.
          </p>
        ) : (
          <NumberField
            label="Dimensions"
            value={settings.embedding.dimensions}
            min={cap.dimensions.minimum}
            max={cap.dimensions.maximum}
            onChange={(dimensions) =>
              onChange({ ...settings, embedding: { dimensions } })
            }
          />
        ))}
      <button
        type="button"
        className="generation-settings-reset"
        onClick={() => onChange(defaultGenerationSettings())}
      >
        Reset to model defaults
      </button>
    </section>
  );
}
