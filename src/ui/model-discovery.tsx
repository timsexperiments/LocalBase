import { useRef, useState, type FormEvent } from "react";
import { z } from "zod";
import { api, type Connection } from "./client";

const kindNames = {
  llm: "Chat",
  stt: "Transcription",
  image: "Image",
} as const;
type Kind = keyof typeof kindNames;

const fitBadges = {
  fits: "Fits",
  tight: "Tight fit",
  "wont-fit": "Won't fit",
} as const;

const choiceSchema = z.object({
  path: z.string(),
  sizeBytes: z.number(),
  quant: z.string().nullable(),
  modelId: z.string(),
  existing: z.enum(["built-in", "remote", "user"]).nullable(),
  fit: z
    .object({
      verdict: z.enum(["fits", "tight", "wont-fit"]),
      minVramGb: z.number(),
      message: z.string(),
    })
    .nullable(),
});
const previewSchema = z.object({
  kind: z.enum(["llm", "stt", "image"]),
  choices: z.array(choiceSchema),
  defaultPath: z.string().nullable(),
});
const hitSchema = z.object({
  repo: z.string(),
  url: z.string(),
  downloads: z.number(),
  license: z.string().nullable(),
  pipelineTag: z.string().nullable(),
  gated: z.boolean(),
  likely: z.object({
    kind: z.enum(["llm", "stt", "image"]).nullable(),
    label: z.string(),
  }),
  preview: previewSchema.nullable(),
  reason: z.string().nullable(),
});
const searchSchema = z.object({
  cached: z.boolean(),
  hits: z.array(hitSchema),
});
const addSchema = z.object({
  modelId: z.string(),
  displayName: z.string(),
  replaced: z.boolean(),
  operation: z
    .object({ state: z.string().optional(), detail: z.string().optional() })
    .passthrough()
    .nullable(),
});
type Hit = z.infer<typeof hitSchema>;
type Choice = z.infer<typeof choiceSchema>;

function compactCount(value: number) {
  if (value >= 1e6) return `${(value / 1e6).toFixed(1)}M`;
  if (value >= 1e3) return `${(value / 1e3).toFixed(1)}k`;
  return String(value);
}

function size(value: number) {
  if (value >= 1024 ** 3) return `${(value / 1024 ** 3).toFixed(1)} GiB`;
  return `${Math.max(1, Math.round(value / 1024 ** 2))} MiB`;
}

function addable(choice: Choice) {
  return choice.existing === null || choice.existing === "user";
}

function choiceLabel(choice: Choice) {
  const parts = [choice.path, size(choice.sizeBytes)];
  if (choice.existing === "user") parts.push("in your catalog");
  else if (choice.existing) parts.push(`already ${choice.existing}`);
  return parts.join(" · ");
}

async function discovery(
  connection: Connection,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): Promise<unknown> {
  const response = await api("/_localbase/model-discovery", connection, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    ...(signal ? { signal } : {}),
  });
  return response.json();
}

function DiscoveryCard({
  hit,
  connection,
  canManage,
  onAdded,
}: {
  hit: Hit;
  connection: Connection;
  canManage: boolean;
  onAdded: (message: string) => void;
}) {
  const [preview, setPreview] = useState(hit.preview);
  const [reason, setReason] = useState(hit.reason);
  const [path, setPath] = useState(hit.preview?.defaultPath ?? "");
  const [busy, setBusy] = useState<"" | "preview" | "add" | "install">("");
  const [error, setError] = useState("");
  const choice = preview?.choices.find((item) => item.path === path);

  async function loadPreview() {
    setBusy("preview");
    setError("");
    try {
      const next = z
        .object({ preview: previewSchema })
        .parse(
          await discovery(connection, { action: "preview", repo: hit.repo }),
        ).preview;
      setPreview(next);
      setReason(null);
      setPath(next.defaultPath ?? "");
    } catch (error) {
      setError(
        error instanceof Error ? error.message : "Could not read the repo.",
      );
    } finally {
      setBusy("");
    }
  }

  async function add(install: boolean) {
    if (!choice) return;
    setBusy(install ? "install" : "add");
    setError("");
    try {
      const added = addSchema.parse(
        await discovery(connection, {
          action: "add",
          repo: hit.repo,
          selector: choice.path,
          install,
        }),
      );
      const failed = added.operation?.state === "failed";
      if (failed)
        setError(
          `Added, but the install did not start: ${added.operation?.detail ?? "unknown error"}`,
        );
      onAdded(
        `${added.replaced ? "Updated" : "Added"} ${added.displayName} (${added.modelId})${install && !failed ? "; installing now" : ""}.`,
      );
      setPreview(
        (current) =>
          current && {
            ...current,
            choices: current.choices.map((item) =>
              item.path === choice.path ? { ...item, existing: "user" } : item,
            ),
          },
      );
    } catch (error) {
      setError(error instanceof Error ? error.message : "Could not add it.");
    } finally {
      setBusy("");
    }
  }

  const [org, name] = hit.repo.split("/");
  const supported = hit.likely.kind !== null;
  return (
    <article className="catalog-card discovery-card">
      <header>
        <h3>
          <a href={hit.url} target="_blank" rel="noopener noreferrer">
            {name}
          </a>
        </h3>
        {hit.gated && <span className="model-badge gated">Gated</span>}
        {choice?.fit && (
          <span
            className={`model-badge fit-${choice.fit.verdict}`}
            title={choice.fit.message}
          >
            {fitBadges[choice.fit.verdict]}
          </span>
        )}
      </header>
      <p className="model-id">{org}</p>
      <p className="model-facts">
        {compactCount(hit.downloads)} downloads ·{" "}
        {hit.license ?? "license not stated"} ·{" "}
        {hit.pipelineTag ?? "no pipeline"} · {hit.likely.label}
      </p>
      {hit.gated && (
        <p className="hint">
          This repository asks you to accept its terms before downloading.{" "}
          <a href={hit.url} target="_blank" rel="noopener noreferrer">
            Accept terms on Hugging Face
          </a>
          , then save a Hugging Face token with{" "}
          <code>local-base configure</code>.
        </p>
      )}
      {preview ? (
        <>
          <label className="discovery-file">
            <span>File</span>
            <select
              value={path}
              disabled={!canManage || busy !== ""}
              onChange={(event) => setPath(event.target.value)}
            >
              {!preview.defaultPath && <option value="">Choose a file…</option>}
              {preview.choices.map((item) => (
                <option
                  key={item.path}
                  value={item.path}
                  disabled={!addable(item)}
                >
                  {choiceLabel(item)}
                </option>
              ))}
            </select>
          </label>
          {choice && (
            <dl className="model-storage">
              <div>
                <dt>Download</dt>
                <dd>{size(choice.sizeBytes)}</dd>
              </div>
              <div>
                <dt>Memory</dt>
                <dd>
                  {choice.fit ? `~${choice.fit.minVramGb.toFixed(1)} GB` : "—"}
                </dd>
              </div>
              <div>
                <dt>Quant</dt>
                <dd>{choice.quant ?? "—"}</dd>
              </div>
              {choice.fit && (
                <div className="model-storage-fit">
                  <dt>Fit</dt>
                  <dd>{choice.fit.message}</dd>
                </div>
              )}
            </dl>
          )}
          <div className="catalog-actions">
            <button
              type="button"
              disabled={!canManage || !choice || !addable(choice) || !!busy}
              onClick={() => void add(false)}
            >
              {busy === "add" ? "Adding…" : "Add"}
            </button>
            <button
              type="button"
              className="install-model"
              disabled={!canManage || !choice || !addable(choice) || !!busy}
              onClick={() => void add(true)}
            >
              {busy === "install" ? "Adding…" : "Add and install"}
            </button>
          </div>
        </>
      ) : (
        <>
          {reason && reason !== hit.likely.label && (
            <p className="hint">{reason}</p>
          )}
          {supported && (!reason || reason.startsWith("Could not")) && (
            <div className="catalog-actions">
              <button
                type="button"
                disabled={!!busy}
                onClick={() => void loadPreview()}
              >
                {busy === "preview" ? "Reading files…" : "Show files"}
              </button>
            </div>
          )}
        </>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
    </article>
  );
}

/** Hugging Face search with the files `models add` would pin. */
export function ModelDiscovery({
  connection,
  canManage,
  onAdded,
}: {
  connection: Connection;
  canManage: boolean;
  onAdded: () => void;
}) {
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState<Kind>("llm");
  const [hits, setHits] = useState<Hit[] | null>(null);
  const [cached, setCached] = useState(false);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [searched, setSearched] = useState(0);
  const abort = useRef<AbortController | null>(null);

  async function search(event?: FormEvent) {
    event?.preventDefault();
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    setSearching(true);
    setError("");
    try {
      const result = searchSchema.parse(
        await discovery(
          connection,
          { action: "search", query: query.trim(), kind },
          controller.signal,
        ),
      );
      if (controller.signal.aborted) return;
      setHits(result.hits);
      setCached(result.cached);
      setSearched((value) => value + 1);
    } catch (error) {
      if (!controller.signal.aborted)
        setError(
          error instanceof Error ? error.message : "Search failed. Try again.",
        );
    } finally {
      if (abort.current === controller) setSearching(false);
    }
  }

  return (
    <section className="model-discovery" aria-label="Find more models">
      <p className="hint">
        Search Hugging Face for chat, transcription, and image models. Add pins
        the exact file and revision to your catalog; LocalBase has not qualified
        these models.
      </p>
      {!canManage && (
        <p className="notice" role="status">
          Read-only access. Sign in with an account authorized to manage models
          to add them.
        </p>
      )}
      <form
        className="discovery-search"
        role="search"
        onSubmit={(event) => void search(event)}
      >
        <input
          type="search"
          aria-label="Search Hugging Face"
          placeholder="Search Hugging Face, e.g. qwen 1.5b"
          value={query}
          maxLength={200}
          onChange={(event) => setQuery(event.target.value)}
        />
        <select
          aria-label="Model kind"
          value={kind}
          onChange={(event) => setKind(event.target.value as Kind)}
        >
          {Object.entries(kindNames).map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
        <button type="submit" className="primary-action" disabled={searching}>
          {searching ? "Searching…" : "Search"}
        </button>
      </form>
      {notice && (
        <div className="notice" role="status">
          <span>{notice}</span>{" "}
          <button type="button" onClick={() => setNotice("")}>
            Dismiss
          </button>
        </div>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {hits && (
        <p className="catalog-count" aria-live="polite">
          {hits.length} results by downloads
          {cached ? " (from the local cache)" : ""}
        </p>
      )}
      {hits && hits.length > 0 && (
        <div className="catalog-grid">
          {hits.map((hit) => (
            <DiscoveryCard
              key={`${searched}:${hit.repo}`}
              hit={hit}
              connection={connection}
              canManage={canManage}
              onAdded={(message) => {
                setNotice(message);
                onAdded();
              }}
            />
          ))}
        </div>
      )}
      {hits && hits.length === 0 && (
        <div className="catalog-empty">
          <p>No {kindNames[kind].toLowerCase()} models match that search.</p>
        </div>
      )}
    </section>
  );
}
