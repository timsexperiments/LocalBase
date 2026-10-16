import { useEffect, useRef, type ReactNode } from "react";
import {
  capabilityLabel,
  capabilitySourceLabels,
  describeCapabilityMismatch,
} from "../catalog/capabilities";
import {
  catalogTierLabels,
  catalogTrustLabels,
  describeFit,
  describeLaunchShape,
  fitLabels,
  formatFileSize,
  formatGb,
  formatTokens,
  licenseLabels,
  memoryFigureLabels,
  modeLabels,
} from "../catalog/model-details";
import { modelMemoryRequirement, modelName, type Model } from "./client";
import { FeedAnnotationDetails, FeedStatusBadge } from "./feed-annotation";

const sourceLabels: Readonly<Record<string, string>> = capabilitySourceLabels;

/** Model status as the management endpoint reports it. */
export type ModelDetailsStatus = Readonly<{
  /** True when this model is its mode's default. */
  active?: boolean;
  downloadBytes?: number | null;
}>;

/** A short revision for display; the full one stays in the tooltip. */
const shortRevision = (revision: string) =>
  /^[0-9a-f]{40}$/.test(revision) ? revision.slice(0, 12) : revision;

/** Only http(s) sources become links. */
function sourceLink(source: string) {
  try {
    const url = new URL(source);
    return url.protocol === "https:" || url.protocol === "http:"
      ? url.href
      : null;
  } catch {
    return null;
  }
}

function contextLength(model: Model) {
  const tokens = model.catalog.contextWindowTokens;
  if (tokens !== null) return formatTokens(tokens);
  return model.catalog.kind === "llm"
    ? "Not listed in the catalog"
    : "Not applicable";
}

/**
 * Everything known about one catalog model: what it is, where it comes from,
 * what it can do and why, and whether it fits this machine. It opens over the
 * catalog as a modal dialog; Escape, the close button, and Back close it.
 */
export function ModelDetails({
  model,
  status,
  close,
  actions,
}: {
  model: Model;
  status: ModelDetailsStatus | undefined;
  close: () => void;
  /** The same install, enable, default, and uninstall controls as the card. */
  actions: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    const previous = document.activeElement;
    // Opening from a shared link happens before the dialog can be shown.
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      dialog?.close();
      if (previous instanceof HTMLElement && previous.isConnected)
        previous.focus();
    };
  }, []);

  const { catalog } = model;
  const memory = catalog.memory;
  const mode =
    catalog.capabilities?.kind === "embedding"
      ? modeLabels.embedding
      : modeLabels[catalog.kind];
  const capabilities =
    catalog.capabilityResolution?.capabilities ??
    catalog.features.map((id) => ({ id, source: "declared" }));
  const mismatches = catalog.capabilityResolution?.mismatches ?? [];
  const link = catalog.source ? sourceLink(catalog.source) : null;
  const titleId = `model-details-title`;

  return (
    <dialog
      ref={ref}
      className="model-details-dialog"
      aria-labelledby={titleId}
      onCancel={(event) => {
        // The catalog is a dialog too; Escape closes only the topmost one.
        event.preventDefault();
        event.stopPropagation();
        close();
      }}
      onClick={(event) => {
        event.stopPropagation();
        if (event.target === event.currentTarget) close();
      }}
    >
      <article className="model-details">
        <header className="model-details-header">
          <button
            type="button"
            className="model-details-back"
            onClick={close}
            aria-label="Back to the catalog"
          >
            <span aria-hidden="true">←</span> Catalog
          </button>
          <h2 id={titleId}>{modelName(model)}</h2>
          <p className="model-id">{model.id}</p>
          <ul className="model-details-badges" aria-label="Status">
            <li className="model-badge">{mode}</li>
            {catalog.qualificationState === "experimental" && (
              <li className="model-badge">Experimental</li>
            )}
            {catalog.trust && catalog.trust !== "qualified" && (
              <li className="model-badge">
                {catalog.trust === "modified"
                  ? "Modified"
                  : catalog.trust === "user"
                    ? "Added by you"
                    : "Community"}
              </li>
            )}
            <li
              className={`model-badge ${model.device.installed ? "installed" : ""}`}
            >
              {model.device.installed ? "Installed" : "Not installed"}
            </li>
            <li className="model-badge">
              {model.device.selected ? "Enabled" : "Disabled"}
            </li>
            {status?.active && <li className="model-badge">Default</li>}
            {memory.fit && (
              <li className={`model-badge fit-${memory.fit.verdict}`}>
                {fitLabels[memory.fit.verdict]}
              </li>
            )}
          </ul>
        </header>

        {catalog.summary && <p className="model-summary">{catalog.summary}</p>}
        {catalog.notes && (
          <p className="model-details-notes">{catalog.notes}</p>
        )}
        {catalog.qualificationState === "experimental" && (
          <p className="hint">
            Not yet qualified: how it behaves on real hardware, and its memory
            estimate, still need checking.
          </p>
        )}

        <div className="model-details-actions">{actions}</div>

        <section aria-labelledby="model-details-about">
          <h3 id="model-details-about">About</h3>
          <dl className="model-details-facts">
            <div>
              <dt>Type</dt>
              <dd>{mode}</dd>
            </div>
            <div>
              <dt>License</dt>
              <dd>
                {catalog.commercialStatus
                  ? licenseLabels[catalog.commercialStatus]
                  : "Not listed"}
              </dd>
            </div>
            {catalog.provider && (
              <div>
                <dt>Publisher</dt>
                <dd>{catalog.provider}</dd>
              </div>
            )}
            {catalog.source && (
              <div>
                <dt>Source</dt>
                <dd>
                  {link ? (
                    <a href={link} target="_blank" rel="noreferrer">
                      {catalog.source.replace(/^https?:\/\//, "")}
                    </a>
                  ) : (
                    catalog.source
                  )}
                  {catalog.revision && (
                    <span className="muted" title={catalog.revision}>
                      {" "}
                      at revision {shortRevision(catalog.revision)}
                    </span>
                  )}
                </dd>
              </div>
            )}
            <div>
              <dt>Context length</dt>
              <dd>{contextLength(model)}</dd>
            </div>
            <div>
              <dt>Quantization</dt>
              <dd>{catalog.quantization}</dd>
            </div>
            {catalog.trust && (
              <div>
                <dt>Catalog</dt>
                <dd>
                  {catalogTrustLabels[catalog.trust]}
                  {catalog.catalogTier
                    ? ` · ${catalogTierLabels[catalog.catalogTier]}`
                    : ""}
                </dd>
              </div>
            )}
            {catalog.catalogFeed && (
              <div>
                <dt>Catalog feed</dt>
                <dd className="model-details-feed">{catalog.catalogFeed}</dd>
              </div>
            )}
          </dl>
        </section>

        <section aria-labelledby="model-details-capabilities">
          <h3 id="model-details-capabilities">What it can do</h3>
          {capabilities.length ? (
            <ul className="model-details-list">
              {capabilities.map(({ id, source }) => (
                <li key={id}>
                  <span>{capabilityLabel(id)}</span>
                  <span className="muted">
                    {sourceLabels[source] ?? source}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="muted">No capabilities listed.</p>
          )}
          {(catalog.unsupportedCapabilities ?? []).length > 0 && (
            <ul
              className="model-details-list"
              aria-label="Verified unsupported"
            >
              {(catalog.unsupportedCapabilities ?? []).map(({ id, reason }) => (
                <li key={id}>
                  <span>{capabilityLabel(id)}</span>
                  <span className="muted">Verified unsupported: {reason}</span>
                </li>
              ))}
            </ul>
          )}
          {mismatches.map((mismatch) => (
            <p className="model-details-note" key={mismatch.id}>
              {describeCapabilityMismatch(mismatch)}
            </p>
          ))}
        </section>

        <section aria-labelledby="model-details-memory">
          <h3 id="model-details-memory">Memory</h3>
          <dl className="model-details-facts">
            <div>
              <dt>Needs at least</dt>
              <dd>{modelMemoryRequirement(model)}</dd>
            </div>
            {memory.reference && (
              <div>
                <dt>{memoryFigureLabels[memory.reference.source]}</dt>
                <dd>
                  {formatGb(memory.reference.bytes)} with{" "}
                  {describeLaunchShape(memory.reference.launchShape)}
                </dd>
              </div>
            )}
            {(memory.observedPeaks ?? []).map((peak) => (
              <div key={peak.launchShape}>
                <dt>Seen on this machine</dt>
                <dd>
                  {formatGb(peak.peakBytes)} with{" "}
                  {describeLaunchShape(peak.launchShape)}
                  <span className="muted">
                    {" "}
                    on {peak.observedAt.slice(0, 10)}
                  </span>
                </dd>
              </div>
            ))}
            {memory.fit && (
              <div>
                <dt>This machine</dt>
                <dd>
                  {memory.fit.systemMemoryGb !== undefined &&
                  memory.fit.headroomGb !== undefined
                    ? describeFit({
                        verdict: memory.fit.verdict,
                        systemMemoryGb: memory.fit.systemMemoryGb,
                        headroomGb: memory.fit.headroomGb,
                      })
                    : fitLabels[memory.fit.verdict]}
                </dd>
              </div>
            )}
          </dl>
        </section>

        {catalog.feedAnnotation && (
          <section aria-labelledby="model-details-feed">
            <h3 id="model-details-feed" className="model-details-feed-heading">
              From the catalog feed{" "}
              <FeedStatusBadge annotation={catalog.feedAnnotation} />
            </h3>
            <FeedAnnotationDetails annotation={catalog.feedAnnotation} />
          </section>
        )}

        <section aria-labelledby="model-details-files">
          <h3 id="model-details-files">Files</h3>
          {catalog.artifacts?.length ? (
            <ul className="model-details-list">
              {catalog.artifacts.map((artifact) => (
                <li key={artifact.filename}>
                  <span className="model-details-filename">
                    {artifact.filename}
                  </span>
                  <span className="muted">
                    {formatFileSize(artifact.sizeBytes)}
                    {artifact.role === "supplementary"
                      ? " · supporting file"
                      : ""}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="muted">
              About {catalog.memory.storageEstimateGb} GB to download.
            </p>
          )}
          {status?.downloadBytes != null && (
            <p className="hint">
              Still to download: {formatFileSize(status.downloadBytes)}.
            </p>
          )}
        </section>
      </article>
    </dialog>
  );
}
