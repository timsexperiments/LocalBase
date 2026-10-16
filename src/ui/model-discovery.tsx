import { useRef, useState, type FormEvent, type ReactNode } from "react";
import { z } from "zod";
import { formatFileSize } from "../catalog/model-details";
import { api, ApiError, feedAnnotationSchema, type Connection } from "./client";
import {
  describeFeedPeak,
  FeedMarks,
  FeedRevisionNote,
  FeedStatusBadge,
  headlineFeedPeak,
} from "./feed-annotation";
import { HfTokenPanel } from "./hf-token";

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
  /** What the catalog feed publishes for this file; absent on older servers. */
  annotation: feedAnnotationSchema.nullable().optional(),
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
  /** Set when the Hub refused (auth) or rate-limited the preview. */
  reasonCode: z.string().optional(),
  /** The Hub's current revision sha, used to match feed annotations. */
  sha: z.string().optional(),
  /** Catalog-feed annotations for the repo; absent on older servers. */
  catalog: z
    .array(
      feedAnnotationSchema.extend({
        displayName: z.string().nullable(),
        installable: z.boolean(),
      }),
    )
    .optional(),
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
type CatalogNote = NonNullable<Hit["catalog"]>[number];
const recipeSchema = z.object({
  modelId: z.string(),
  displayName: z.string(),
  operation: z
    .object({ state: z.string().optional(), detail: z.string().optional() })
    .passthrough()
    .nullable(),
});

/** Error codes a saved Hugging Face token can fix. */
const tokenCodes = new Set(["hub_auth_required", "hub_rate_limited"]);

type Failure = { message: string; token: boolean };

function failure(error: unknown, fallback: string): Failure {
  return {
    message: error instanceof Error ? error.message : fallback,
    token: error instanceof ApiError && tokenCodes.has(error.code ?? ""),
  };
}

/** An error line, with a link to the token panel when a token would help. */
function FailureLine({
  failure,
  onToken,
}: {
  failure: Failure;
  onToken?: (() => void) | undefined;
}) {
  return (
    <p className="error discovery-error" role="alert">
      <span>{failure.message}</span>
      {failure.token && onToken && <TokenLink onToken={onToken} />}
    </p>
  );
}

function TokenLink({
  onToken,
  children = "Add a Hugging Face token",
}: {
  onToken: () => void;
  children?: ReactNode;
}) {
  return (
    <button
      type="button"
      className="link-button"
      aria-controls="hf-token"
      onClick={onToken}
    >
      {children}
    </button>
  );
}

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
  onToken,
}: {
  hit: Hit;
  connection: Connection;
  canManage: boolean;
  onAdded: (message: string) => void;
  /** Opens the token panel; absent without models:manage. */
  onToken?: (() => void) | undefined;
}) {
  const [preview, setPreview] = useState(hit.preview);
  const [reason, setReason] = useState(hit.reason);
  const [path, setPath] = useState(hit.preview?.defaultPath ?? "");
  const [busy, setBusy] = useState<
    "" | "preview" | "add" | "install" | "recipe"
  >("");
  const [reasonCode, setReasonCode] = useState(hit.reasonCode);
  const [error, setError] = useState<Failure | null>(null);
  const choice = preview?.choices.find((item) => item.path === path);
  const notes = hit.catalog ?? [];
  const recipes = notes.filter((note) => note.recipe);
  const annotation = choice?.annotation ?? null;
  const peak = annotation ? headlineFeedPeak(annotation) : undefined;

  async function installRecipe(note: CatalogNote) {
    if (!note.recipe) return;
    setBusy("recipe");
    setError(null);
    try {
      const started = recipeSchema.parse(
        await discovery(connection, {
          action: "install-recipe",
          modelId: note.recipe.modelId,
        }),
      );
      if (started.operation?.state === "failed")
        setError({
          message: `The install did not start: ${started.operation.detail ?? "unknown error"}`,
          token: false,
        });
      else onAdded(`Installing ${started.displayName} (${started.modelId}).`);
    } catch (error) {
      setError(failure(error, "Could not start the install."));
    } finally {
      setBusy("");
    }
  }

  async function loadPreview() {
    setBusy("preview");
    setError(null);
    try {
      const next = z
        .object({ preview: previewSchema })
        .parse(
          await discovery(connection, { action: "preview", repo: hit.repo }),
        ).preview;
      setPreview(next);
      setReason(null);
      setReasonCode(undefined);
      setPath(next.defaultPath ?? "");
    } catch (error) {
      setError(failure(error, "Could not read the repo."));
    } finally {
      setBusy("");
    }
  }

  async function add(install: boolean) {
    if (!choice) return;
    setBusy(install ? "install" : "add");
    setError(null);
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
        setError({
          message: `Added, but the install did not start: ${added.operation?.detail ?? "unknown error"}`,
          token: false,
        });
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
      setError(failure(error, "Could not add it."));
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
        <FeedStatusBadge
          annotation={annotation ?? notes[0]}
          showRevision={!!annotation || !!hit.sha}
        />
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
          , then{" "}
          {onToken ? (
            <TokenLink onToken={onToken}>save a Hugging Face token</TokenLink>
          ) : (
            <>
              save a Hugging Face token with <code>local-base configure</code>
            </>
          )}
          .
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
              {peak && (
                <div className="model-storage-fit">
                  <dt>Measured peak</dt>
                  <dd>
                    {describeFeedPeak(peak)}
                    <span className="muted"> · {peak.hardware}</span>
                  </dd>
                </div>
              )}
              {choice.fit && (
                <div className="model-storage-fit">
                  <dt>Fit</dt>
                  <dd>{choice.fit.message}</dd>
                </div>
              )}
            </dl>
          )}
          {annotation && (
            <>
              <FeedRevisionNote annotation={annotation} />
              <FeedMarks annotation={annotation} compact />
            </>
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
            <p className="hint">
              {reason}
              {reasonCode && tokenCodes.has(reasonCode) && onToken && (
                <>
                  {" "}
                  <TokenLink onToken={onToken} />
                </>
              )}
            </p>
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
      {recipes.length > 0 && (
        <ul className="discovery-recipes" aria-label="LocalBase recipes">
          {recipes.map((note) => (
            <li key={note.recipe!.modelId}>
              <div>
                <strong>{note.displayName ?? note.recipe!.modelId}</strong>
                <span className="muted">
                  {" "}
                  · {note.recipe!.fileCount} files ·{" "}
                  {formatFileSize(note.recipe!.totalBytes)}
                  {headlineFeedPeak(note)
                    ? ` · peak ${describeFeedPeak(headlineFeedPeak(note)!)}`
                    : ""}
                </span>
                {note.file && (
                  <code className="discovery-recipe-file">{note.file}</code>
                )}
              </div>
              <button
                type="button"
                className="install-model"
                disabled={!canManage || !note.installable || !!busy}
                title={
                  note.installable
                    ? "Installs the catalog entry with every file pinned by sha256."
                    : "This LocalBase version's catalog does not have this entry yet."
                }
                onClick={() => void installRecipe(note)}
              >
                {busy === "recipe" ? "Starting…" : "Install LocalBase recipe"}
              </button>
            </li>
          ))}
        </ul>
      )}
      {error && <FailureLine failure={error} onToken={onToken} />}
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
  const [error, setError] = useState<Failure | null>(null);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [tokenFocus, setTokenFocus] = useState(0);
  const openToken = canManage
    ? () => {
        setTokenOpen(true);
        setTokenFocus((value) => value + 1);
      }
    : undefined;
  const [notice, setNotice] = useState("");
  const [searched, setSearched] = useState(0);
  const abort = useRef<AbortController | null>(null);

  async function search(event?: FormEvent) {
    event?.preventDefault();
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    setSearching(true);
    setError(null);
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
        setError(failure(error, "Search failed. Try again."));
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
      {canManage && (
        <HfTokenPanel
          connection={connection}
          open={tokenOpen}
          onToggle={setTokenOpen}
          focusKey={tokenFocus}
        />
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
      {error && <FailureLine failure={error} onToken={openToken} />}
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
              onToken={openToken}
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
