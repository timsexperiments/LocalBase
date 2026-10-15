import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  modelHasCapability,
  modelName,
  type Media,
  type Message,
  type Model,
} from "./client";
import { DictationButton, appendDictation } from "./dictation";
import {
  GenerationSettingsFields,
  type GenerationSettings,
  type ImageSize,
} from "./generation-settings";

export type StudioRequest = {
  /** PNG cropped to `size`, sent to the edits endpoint. */
  source?: Blob;
  /** Data URL of the cropped source, kept with the results for comparison. */
  sourceUrl?: string;
  /** Black image with white strokes over the regions to repaint. */
  mask?: Blob;
  /** Base64 PNG, without a data URL prefix, at the video profile size. */
  firstFrame?: string;
  size?: ImageSize;
};
type Source = { url: string; image: HTMLImageElement };
type Result = {
  id: string;
  label: string;
  media: Media;
  prompt: string;
  source?: string;
};

const maxSourceBytes = 25 * 1024 * 1024;
const maxFirstFrameBytes = 4 * 1024 * 1024;

export function canEditImages(model: Model | undefined) {
  return model ? modelHasCapability(model, "image-to-image") : false;
}
export type ImageMode = "text-to-image" | "image-to-image" | "inpaint";
/** Which image endpoint and mode a studio request uses with this model. */
export function imageRequestMode(
  request: Pick<StudioRequest, "source" | "mask"> | undefined,
  model: Model | undefined,
): ImageMode {
  if (!request?.source || !canEditImages(model)) return "text-to-image";
  return request.mask ? "inpaint" : "image-to-image";
}
const imageNouns: Record<ImageMode, string> = {
  "text-to-image": "Generated",
  "image-to-image": "Transformed",
  inpaint: "Inpainted",
};
/** Summary text for an image result, e.g. "Inpainted 2 images". */
export function imageResultText(mode: ImageMode, count: number) {
  return count > 1
    ? `${imageNouns[mode]} ${count} images`
    : `${imageNouns[mode]} image`;
}
export const imageModeLabels: Record<ImageMode, string> = {
  "text-to-image": "Text to image",
  "image-to-image": "Image to image",
  inpaint: "Inpaint",
};
/** Label for one image result card, naming the mode that produced it. */
export function imageResultLabel(
  mode: ImageMode,
  index: number,
  count: number,
) {
  return count > 1
    ? `${imageModeLabels[mode]} ${index + 1}`
    : imageModeLabels[mode];
}
export function acceptsFirstFrame(model: Model | undefined) {
  const cap = model?.catalog.capabilities;
  return (
    cap?.kind === "video" &&
    (cap.mode === "ti2v" ||
      (cap.mode === "t2v" &&
        (model?.catalog.inputModalities.includes("image") ?? false)))
  );
}
/** Picks the supported size whose aspect ratio is closest to the source. */
export function sizeForAspect(width: number, height: number): ImageSize {
  const ratio = width / height;
  return ratio > 1.2 ? "768x512" : ratio < 1 / 1.2 ? "512x768" : "512x512";
}
export function studioResults(messages: Message[]): Result[] {
  return messages.flatMap((message, index) => {
    if (message.role !== "assistant") return [];
    const prompt = messages[index - 1]?.text ?? "";
    return (message.artifacts ?? []).flatMap((artifact) =>
      artifact.state === "complete"
        ? [
            {
              id: artifact.id,
              label: artifact.label,
              media: artifact.media,
              prompt,
              source: message.source,
            },
          ]
        : [],
    );
  });
}

function loadImage(url: string) {
  return new Promise<HTMLImageElement>((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve(image);
    image.onerror = () =>
      reject(new Error("That file could not be read as an image."));
    image.src = url;
  });
}
function readDataUrl(file: Blob) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(new Error("The file could not be read."));
    reader.readAsDataURL(file);
  });
}
/** Scales and center-crops `source` to fill exactly `width` × `height`. */
function cover(
  source: CanvasImageSource,
  sourceWidth: number,
  sourceHeight: number,
  width: number,
  height: number,
) {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext("2d");
  if (!context) throw new Error("This browser cannot prepare images.");
  const scale = Math.max(width / sourceWidth, height / sourceHeight);
  const cropWidth = width / scale;
  const cropHeight = height / scale;
  context.imageSmoothingQuality = "high";
  context.drawImage(
    source,
    (sourceWidth - cropWidth) / 2,
    (sourceHeight - cropHeight) / 2,
    cropWidth,
    cropHeight,
    0,
    0,
    width,
    height,
  );
  return canvas;
}
function pngBlob(canvas: HTMLCanvasElement) {
  return new Promise<Blob>((resolve, reject) =>
    canvas.toBlob(
      (blob) =>
        blob ? resolve(blob) : reject(new Error("Could not encode the image.")),
      "image/png",
    ),
  );
}
function exportMask(strokes: HTMLCanvasElement) {
  const white = document.createElement("canvas");
  white.width = strokes.width;
  white.height = strokes.height;
  const whiteContext = white.getContext("2d");
  const mask = document.createElement("canvas");
  mask.width = strokes.width;
  mask.height = strokes.height;
  const maskContext = mask.getContext("2d");
  if (!whiteContext || !maskContext)
    throw new Error("This browser cannot prepare masks.");
  whiteContext.drawImage(strokes, 0, 0);
  whiteContext.globalCompositeOperation = "source-in";
  whiteContext.fillStyle = "#fff";
  whiteContext.fillRect(0, 0, white.width, white.height);
  maskContext.fillStyle = "#000";
  maskContext.fillRect(0, 0, mask.width, mask.height);
  maskContext.drawImage(white, 0, 0);
  return mask;
}
function extension(media: Media) {
  return media.kind === "image"
    ? "png"
    : media.kind === "audio"
      ? "wav"
      : "mp4";
}

export function Studio({
  mode,
  model,
  messages,
  busy,
  progress,
  prompt,
  setPrompt,
  settings,
  onSettings,
  canDictate,
  canGenerate,
  notices,
  onGenerate,
  onStop,
  onChooseModel,
}: {
  mode: "image" | "video";
  model: Model | undefined;
  messages: Message[];
  busy: boolean;
  progress: string;
  prompt: string;
  setPrompt: (prompt: string | ((prompt: string) => string)) => void;
  settings: GenerationSettings;
  onSettings: (settings: GenerationSettings) => void;
  canDictate: boolean;
  canGenerate: boolean;
  notices: ReactNode;
  onGenerate: (request: StudioRequest) => void;
  onStop: () => void;
  onChooseModel: () => void;
}) {
  const [source, setSource] = useState<Source | null>(null);
  const [sourceError, setSourceError] = useState("");
  const [view, setView] = useState<"source" | "result">("result");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [comparing, setComparing] = useState(false);
  const [split, setSplit] = useState(50);
  const [tool, setTool] = useState<"brush" | "erase">("brush");
  const [brush, setBrush] = useState(40);
  const [masked, setMasked] = useState(false);
  const [dragging, setDragging] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const maskCanvas = useRef<HTMLCanvasElement>(null);
  const resultVideo = useRef<HTMLVideoElement>(null);
  const lastPoint = useRef<{ x: number; y: number } | null>(null);
  const cap = model?.catalog.capabilities;
  const sourceSupported =
    mode === "image" ? canEditImages(model) : acceptsFirstFrame(model);
  const usesSource = sourceSupported && source !== null;
  const results = studioResults(messages);
  const newest = results.at(-1);
  const selected =
    results.find((result) => result.id === selectedId) ?? newest ?? null;
  const showingSource = source !== null && (view === "source" || !selected);
  const strength = settings.image.strength ?? 0.75;
  const batch = settings.image.n ?? 1;

  // Show each new result as soon as it arrives.
  useEffect(() => {
    if (!newest) return;
    setSelectedId(newest.id);
    setView("result");
  }, [newest?.id]);
  useEffect(() => setComparing(false), [selected?.id]);

  async function chooseSource(file: Blob) {
    setSourceError("");
    if (!file.type.startsWith("image/")) {
      setSourceError("Choose a PNG, JPEG or WebP image.");
      return;
    }
    if (file.size > maxSourceBytes) {
      setSourceError("Choose an image smaller than 25 MiB.");
      return;
    }
    try {
      await useSource(await readDataUrl(file));
    } catch (cause) {
      setSourceError(
        cause instanceof Error ? cause.message : "Could not read the image.",
      );
    }
  }
  async function useSource(url: string) {
    const image = await loadImage(url);
    setSource({ url, image });
    setMasked(false);
    setView("source");
    const canvas = maskCanvas.current;
    if (canvas) {
      const scale = Math.min(1, 1024 / Math.max(image.width, image.height));
      canvas.width = Math.round(image.width * scale);
      canvas.height = Math.round(image.height * scale);
    }
  }
  function useFrame() {
    const video = resultVideo.current;
    if (!video?.videoWidth) return;
    const canvas = cover(
      video,
      video.videoWidth,
      video.videoHeight,
      video.videoWidth,
      video.videoHeight,
    );
    void useSource(canvas.toDataURL("image/png")).catch(() =>
      setSourceError("Could not capture that frame."),
    );
  }
  function removeSource() {
    setSource(null);
    setMasked(false);
    setSourceError("");
    setView("result");
  }
  function clearMask() {
    const canvas = maskCanvas.current;
    canvas?.getContext("2d")?.clearRect(0, 0, canvas.width, canvas.height);
    setMasked(false);
  }
  // Size the mask canvas once the source element exists.
  useEffect(() => {
    const canvas = maskCanvas.current;
    if (!canvas || !source) return;
    const scale = Math.min(
      1,
      1024 / Math.max(source.image.width, source.image.height),
    );
    const width = Math.round(source.image.width * scale);
    const height = Math.round(source.image.height * scale);
    if (canvas.width !== width || canvas.height !== height) {
      canvas.width = width;
      canvas.height = height;
    }
  }, [source]);
  // Paste an image anywhere in the studio to use it as the source.
  useEffect(() => {
    if (!sourceSupported) return;
    const paste = (event: ClipboardEvent) => {
      const file = Array.from(event.clipboardData?.files ?? []).find((item) =>
        item.type.startsWith("image/"),
      );
      if (!file || busy) return;
      event.preventDefault();
      void chooseSource(file);
    };
    document.addEventListener("paste", paste);
    return () => document.removeEventListener("paste", paste);
  }, [sourceSupported, busy]);

  function paint(event: React.PointerEvent<HTMLCanvasElement>) {
    const canvas = event.currentTarget;
    const context = canvas.getContext("2d");
    if (!context) return;
    const rect = canvas.getBoundingClientRect();
    const scale = canvas.width / rect.width;
    const point = {
      x: (event.clientX - rect.left) * scale,
      y: (event.clientY - rect.top) * scale,
    };
    const from = lastPoint.current ?? point;
    context.globalCompositeOperation =
      tool === "erase" ? "destination-out" : "source-over";
    context.strokeStyle = "#ff3d7f";
    context.lineWidth = brush * scale;
    context.lineCap = "round";
    context.lineJoin = "round";
    context.beginPath();
    context.moveTo(from.x, from.y);
    context.lineTo(point.x, point.y);
    context.stroke();
    lastPoint.current = point;
    if (tool === "brush") setMasked(true);
  }

  async function generate() {
    if (!canGenerate || busy || !prompt.trim()) return;
    setSourceError("");
    try {
      if (!usesSource || !source) {
        onGenerate({});
        return;
      }
      const { image } = source;
      if (mode === "video") {
        if (cap?.kind !== "video") return;
        const frame = cover(
          image,
          image.width,
          image.height,
          cap.width,
          cap.height,
        );
        const url = frame.toDataURL("image/png");
        const data = url.slice(url.indexOf(",") + 1);
        if ((data.length * 3) / 4 > maxFirstFrameBytes)
          throw new Error(
            "The first frame is larger than 4 MiB as PNG. Try a simpler image.",
          );
        onGenerate({ firstFrame: data, sourceUrl: url });
        return;
      }
      const size =
        settings.image.size ?? sizeForAspect(image.width, image.height);
      const [width, height] = size.split("x").map(Number) as [number, number];
      const cropped = cover(image, image.width, image.height, width, height);
      const strokes = maskCanvas.current;
      const mask =
        masked && strokes
          ? await pngBlob(
              cover(
                exportMask(strokes),
                strokes.width,
                strokes.height,
                width,
                height,
              ),
            )
          : undefined;
      onGenerate({
        source: await pngBlob(cropped),
        sourceUrl: cropped.toDataURL("image/png"),
        mask,
        size,
      });
    } catch (cause) {
      setSourceError(
        cause instanceof Error ? cause.message : "Could not prepare the image.",
      );
    }
  }

  const sourceLabel = mode === "image" ? "Source image" : "First frame";
  const target =
    mode === "video" && cap?.kind === "video"
      ? `${cap.width}x${cap.height}`
      : source
        ? (settings.image.size ??
          sizeForAspect(source.image.width, source.image.height))
        : "";
  return (
    <div className={`studio studio-${mode}`}>
      <section
        className={`studio-stage${dragging ? " dragging" : ""}`}
        aria-label={mode === "image" ? "Image canvas" : "Video preview"}
        onDragOver={(event) => {
          if (!sourceSupported || busy) return;
          event.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(event) => {
          setDragging(false);
          if (!sourceSupported || busy) return;
          event.preventDefault();
          const file = event.dataTransfer.files[0];
          if (file) void chooseSource(file);
        }}
      >
        <div className="studio-frame" hidden={!showingSource}>
          {source && (
            <div className="studio-layer">
              <img src={source.url} alt={sourceLabel} draggable={false} />
              {mode === "image" && (
                <canvas
                  ref={maskCanvas}
                  className={`studio-mask ${tool}`}
                  aria-label="Mask: paint over the areas to repaint"
                  hidden={!canEditImages(model)}
                  onPointerDown={(event) => {
                    if (busy) return;
                    event.currentTarget.setPointerCapture(event.pointerId);
                    lastPoint.current = null;
                    paint(event);
                  }}
                  onPointerMove={(event) => {
                    if (event.buttons && lastPoint.current) paint(event);
                  }}
                  onPointerUp={() => (lastPoint.current = null)}
                  onPointerCancel={() => (lastPoint.current = null)}
                />
              )}
            </div>
          )}
        </div>
        {!showingSource && selected && (
          <div className="studio-frame">
            {selected.media.kind === "image" ? (
              <div className="studio-layer">
                <img
                  src={selected.media.url}
                  alt={`${selected.label}: ${selected.prompt}`}
                />
                {comparing && selected.source && (
                  <>
                    <img
                      className="studio-before"
                      src={selected.source}
                      alt="Source before editing"
                      style={{ clipPath: `inset(0 ${100 - split}% 0 0)` }}
                    />
                    <span
                      className="studio-split"
                      style={{ left: `${split}%` }}
                      aria-hidden="true"
                    />
                  </>
                )}
              </div>
            ) : selected.media.kind === "video" ? (
              <video
                ref={resultVideo}
                controls
                playsInline
                loop
                src={selected.media.url}
                aria-label={`Generated: ${selected.prompt}`}
              />
            ) : null}
          </div>
        )}
        {!showingSource && !selected && !busy && (
          <div className="studio-placeholder">
            <h1>{mode === "image" ? "Image studio" : "Video studio"}</h1>
            <p>
              {mode === "image"
                ? sourceSupported
                  ? "Describe an image to generate, or drop, paste or upload a picture to edit."
                  : "Describe an image to generate it."
                : sourceSupported
                  ? "Describe a clip, and optionally add a first frame to animate."
                  : "Describe a clip to generate it."}
            </p>
            {sourceSupported && (
              <button type="button" onClick={() => fileInput.current?.click()}>
                Upload {sourceLabel.toLowerCase()}
              </button>
            )}
          </div>
        )}
        {busy && (
          <div className="studio-busy" role="status">
            <span className="studio-spinner" aria-hidden="true" />
            {progress ||
              (mode === "image" ? "Generating image…" : "Generating video…")}
          </div>
        )}
        {dragging && (
          <div className="studio-drop" aria-hidden="true">
            Drop to use as {sourceLabel.toLowerCase()}
          </div>
        )}
      </section>

      <section className="studio-toolbar" aria-label="Canvas tools">
        {showingSource && mode === "image" && canEditImages(model) ? (
          <>
            <div
              className="studio-segmented"
              role="group"
              aria-label="Mask tool"
            >
              <button
                type="button"
                aria-pressed={tool === "brush"}
                onClick={() => setTool("brush")}
              >
                Brush
              </button>
              <button
                type="button"
                aria-pressed={tool === "erase"}
                onClick={() => setTool("erase")}
              >
                Erase
              </button>
            </div>
            <label className="studio-range">
              <span>Size</span>
              <input
                type="range"
                min={4}
                max={160}
                value={brush}
                onChange={(event) =>
                  setBrush(event.currentTarget.valueAsNumber)
                }
              />
            </label>
            <button type="button" disabled={!masked} onClick={clearMask}>
              Clear mask
            </button>
            <span className="studio-tip">
              {masked
                ? "Only the painted area will change."
                : "Paint to change only part of the image."}
            </span>
            {selected && (
              <button
                type="button"
                className="studio-push"
                onClick={() => setView("result")}
              >
                View result
              </button>
            )}
          </>
        ) : selected && !showingSource ? (
          <>
            <span className="studio-result-mode">{selected.label}</span>
            {selected.media.kind === "image" && sourceSupported && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void useSource(selected.media.url)}
              >
                Use as source
              </button>
            )}
            {selected.media.kind === "video" && sourceSupported && (
              <button type="button" disabled={busy} onClick={useFrame}>
                Use this frame
              </button>
            )}
            {selected.media.kind === "image" && selected.source && (
              <button
                type="button"
                aria-pressed={comparing}
                onClick={() => setComparing((value) => !value)}
              >
                Compare
              </button>
            )}
            {comparing && (
              <label className="studio-range">
                <span>Before</span>
                <input
                  type="range"
                  min={0}
                  max={100}
                  value={split}
                  aria-label="Before and after split"
                  onChange={(event) =>
                    setSplit(event.currentTarget.valueAsNumber)
                  }
                />
              </label>
            )}
            <a
              className="download"
              href={selected.media.url}
              download={`localbase-${selected.id.slice(0, 8)}.${extension(selected.media)}`}
            >
              Download
            </a>
            {source && (
              <button
                type="button"
                className="studio-push"
                onClick={() => setView("source")}
              >
                Edit source
              </button>
            )}
          </>
        ) : (
          results.length === 0 &&
          messages.length > 0 && (
            <span className="studio-tip">
              Earlier results are not kept after reloading.
            </span>
          )
        )}
      </section>

      {results.length > 0 && (
        <section className="studio-gallery" aria-label="Results">
          <ul>
            {[...results].reverse().map((result) => (
              <li key={result.id}>
                <button
                  type="button"
                  aria-pressed={!showingSource && result.id === selected?.id}
                  title={`${result.label}: ${result.prompt}`}
                  onClick={() => {
                    setSelectedId(result.id);
                    setView("result");
                  }}
                >
                  {result.media.kind === "image" ? (
                    <img
                      src={result.media.url}
                      alt={`${result.label}: ${result.prompt}`}
                    />
                  ) : (
                    <video src={result.media.url} muted preload="metadata" />
                  )}
                  <span className="studio-result-label">{result.label}</span>
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <aside className="studio-panel" aria-label="Generation controls">
        <button
          type="button"
          className="model-picker studio-model"
          disabled={busy}
          onClick={onChooseModel}
          aria-label={`Choose model: ${model ? modelName(model) : "none selected"}`}
        >
          <span className="status-dot ready" aria-hidden="true" />
          <span>{model ? modelName(model) : "Choose model"}</span>
          <span aria-hidden="true">⌄</span>
        </button>
        <div className="studio-group studio-prompt-group">
          <label className="generation-settings-field">
            <span>Prompt</span>
            <textarea
              className="generation-settings-control studio-prompt"
              value={prompt}
              rows={4}
              placeholder={
                mode === "image"
                  ? usesSource
                    ? "Describe the edit, e.g. make it a watercolor painting"
                    : "A lighthouse on a cliff at sunset, oil painting"
                  : usesSource
                    ? "Describe the motion, e.g. waves rolling in slowly"
                    : "Describe the video, e.g. a paper boat on a rainy street"
              }
              onChange={(event) => setPrompt(event.target.value)}
              onKeyDown={(event) => {
                if (
                  event.key === "Enter" &&
                  (event.metaKey || event.ctrlKey) &&
                  !event.nativeEvent.isComposing
                ) {
                  event.preventDefault();
                  void generate();
                }
              }}
            />
          </label>
          {canDictate && (
            <DictationButton
              label="prompt"
              disabled={busy}
              onText={(text) =>
                setPrompt((value) => appendDictation(value, text))
              }
            />
          )}
        </div>

        <div className="studio-group">
          <span className="studio-heading">{sourceLabel}</span>
          <input
            ref={fileInput}
            type="file"
            hidden
            accept="image/png,image/jpeg,image/webp"
            aria-label={`Choose ${sourceLabel.toLowerCase()}`}
            onChange={(event) => {
              const file = event.currentTarget.files?.[0];
              event.currentTarget.value = "";
              if (file) void chooseSource(file);
            }}
          />
          {!sourceSupported ? (
            <p className="generation-settings-hint">
              {mode === "image"
                ? "This model generates from text only. Choose a model with image-to-image support to edit pictures."
                : "This model generates from text only. Choose an image-to-video model to animate a picture."}{" "}
              <button type="button" className="link" onClick={onChooseModel}>
                Choose model
              </button>
            </p>
          ) : source ? (
            <div className="studio-source">
              <button
                type="button"
                className="studio-thumb"
                aria-pressed={showingSource}
                onClick={() => setView("source")}
                title={`Show ${sourceLabel.toLowerCase()}`}
              >
                <img src={source.url} alt="" />
              </button>
              <span>
                {source.image.width} × {source.image.height}
                {target !== `${source.image.width}x${source.image.height}` &&
                  ` · cropped to ${target.replace("x", " × ")}`}
              </span>
              <button
                type="button"
                disabled={busy}
                onClick={() => fileInput.current?.click()}
              >
                Replace
              </button>
              <button type="button" disabled={busy} onClick={removeSource}>
                Remove
              </button>
            </div>
          ) : (
            <button
              type="button"
              className="studio-dropzone"
              disabled={busy}
              onClick={() => fileInput.current?.click()}
            >
              <strong>Add {sourceLabel.toLowerCase()}</strong>
              <span>Drop, paste or browse · optional</span>
            </button>
          )}
          {sourceError && (
            <p className="generation-settings-hint" role="alert">
              {sourceError}
            </p>
          )}
          {mode === "image" && usesSource && (
            <label className="studio-range">
              <span>Strength</span>
              <input
                type="range"
                min={0.05}
                max={1}
                step={0.05}
                value={strength}
                onChange={(event) =>
                  onSettings({
                    ...settings,
                    image: {
                      ...settings.image,
                      strength: event.currentTarget.valueAsNumber,
                    },
                  })
                }
              />
              <output>{strength.toFixed(2)}</output>
            </label>
          )}
          {mode === "image" && usesSource && (
            <p className="generation-settings-hint">
              Low strength keeps the source; high strength follows the prompt.
            </p>
          )}
        </div>

        {mode === "image" && (
          <div className="studio-group">
            <span className="studio-heading">Images per run</span>
            <div
              className="studio-segmented"
              role="group"
              aria-label="Images per run"
            >
              {[1, 2, 3, 4].map((count) => (
                <button
                  key={count}
                  type="button"
                  aria-pressed={batch === count}
                  onClick={() =>
                    onSettings({
                      ...settings,
                      image: { ...settings.image, n: count },
                    })
                  }
                >
                  {count}
                </button>
              ))}
            </div>
          </div>
        )}

        {notices}

        {busy ? (
          <button type="button" className="studio-generate" onClick={onStop}>
            Stop
          </button>
        ) : (
          <button
            type="button"
            className="studio-generate"
            disabled={!canGenerate || !prompt.trim()}
            onClick={() => void generate()}
          >
            {mode === "image"
              ? usesSource
                ? masked
                  ? "Inpaint"
                  : "Transform"
                : batch > 1
                  ? `Generate ${batch}`
                  : "Generate"
              : usesSource
                ? "Animate"
                : "Generate video"}
            <kbd>⌘↵</kbd>
          </button>
        )}

        {model && (
          <details className="studio-advanced">
            <summary>Advanced settings</summary>
            <GenerationSettingsFields
              mode={mode}
              model={model}
              settings={settings}
              canDictate={canDictate}
              onChange={onSettings}
            />
          </details>
        )}
      </aside>
    </div>
  );
}
