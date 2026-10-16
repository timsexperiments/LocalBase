import {
  describeLaunchShape,
  formatFileSize,
  formatGb,
} from "../catalog/model-details";
import type { FeedAnnotationSummary } from "./client";

type Peak = FeedAnnotationSummary["memoryPeaks"][number];

export const feedStatusLabels = {
  qualified: "Qualified by LocalBase",
  verified: "Verified by LocalBase",
} as const;

const feedStatusHints = {
  qualified:
    "LocalBase tested this exact revision and ships it as a supported model.",
  verified:
    "LocalBase checked this exact revision and published what it found, but does not ship it as a qualified model.",
} as const;

/** The largest published peak, the figure worth headlining. */
export function headlineFeedPeak(
  annotation: FeedAnnotationSummary,
): Peak | undefined {
  return annotation.memoryPeaks.reduce<Peak | undefined>(
    (best, peak) => (!best || peak.peakBytes > best.peakBytes ? peak : best),
    undefined,
  );
}

/** A launch shape in words; shapes the shared helper does not know stay readable. */
function shapeLabel(shape: string): string {
  const described = describeLaunchShape(shape);
  return described === shape ? shape.split(";").join(", ") : described;
}

export function describeFeedPeak(peak: Peak): string {
  return `${formatGb(peak.peakBytes)} with ${shapeLabel(peak.launchShape)}`;
}

/** The "Qualified/Verified by LocalBase" badge, or nothing without a status. */
export function FeedStatusBadge({
  annotation,
  showRevision = true,
}: {
  annotation: FeedAnnotationSummary | null | undefined;
  /** Say so when the annotation describes another revision. */
  showRevision?: boolean;
}) {
  if (!annotation?.status) return null;
  return (
    <span
      className={`model-badge feed-status feed-${annotation.status}`}
      title={feedStatusHints[annotation.status]}
    >
      {feedStatusLabels[annotation.status]}
      {annotation.revisionMatch || !showRevision ? "" : " (other revision)"}
    </span>
  );
}

/** A one-line note when the annotation describes another revision. */
export function FeedRevisionNote({
  annotation,
}: {
  annotation: FeedAnnotationSummary;
}) {
  if (annotation.revisionMatch) return null;
  return (
    <p className="model-details-note feed-revision-note">
      The catalog feed describes revision{" "}
      <code title={annotation.revision}>
        {annotation.revision.slice(0, 12)}
      </code>
      , not the one pinned here, so its marks are shown but not applied.
    </p>
  );
}

/** Verified-unsupported marks with their reasons. */
export function FeedMarks({
  annotation,
  compact = false,
}: {
  annotation: FeedAnnotationSummary;
  compact?: boolean;
}) {
  if (annotation.unsupportedCapabilities.length === 0) return null;
  return (
    <ul
      className={`model-details-list feed-marks${compact ? " compact" : ""}`}
      aria-label="Verified unsupported (catalog feed)"
    >
      {annotation.unsupportedCapabilities.map((mark) => (
        <li key={mark.id}>
          <span>
            {mark.label}
            {!mark.applied && (
              <span className="muted feed-mark-state">
                {annotation.revisionMatch
                  ? " · not applied: your entry declares it"
                  : " · not applied"}
              </span>
            )}
          </span>
          <span className="muted">Verified unsupported: {mark.reason}</span>
        </li>
      ))}
    </ul>
  );
}

/** The "From the catalog feed" facts in the model details dialog. */
export function FeedAnnotationDetails({
  annotation,
}: {
  annotation: FeedAnnotationSummary;
}) {
  const recipe = annotation.recipe;
  return (
    <>
      <FeedRevisionNote annotation={annotation} />
      <dl className="model-details-facts">
        <div>
          <dt>Status</dt>
          <dd>
            {annotation.status
              ? feedStatusLabels[annotation.status]
              : "Not qualified or verified"}
          </dd>
        </div>
        <div>
          <dt>Published for</dt>
          <dd className="model-details-feed">
            {annotation.repo}
            {annotation.file ? ` · ${annotation.file}` : ""}
            <span className="muted" title={annotation.revision}>
              {" "}
              at {annotation.revision.slice(0, 12)}
            </span>
          </dd>
        </div>
        {annotation.memoryPeaks.map((peak) => (
          <div key={`${peak.variant}:${peak.launchShape}`}>
            <dt>Measured peak</dt>
            <dd>
              {describeFeedPeak(peak)}
              <span className="muted">
                {" "}
                · {peak.variant} on {peak.hardware}, {peak.measuredAt}
              </span>
            </dd>
          </div>
        ))}
        {recipe && (
          <div>
            <dt>Recipe</dt>
            <dd>
              {recipe.fileCount} files, {formatFileSize(recipe.totalBytes)},
              pinned by sha256
            </dd>
          </div>
        )}
      </dl>
      <FeedMarks annotation={annotation} />
    </>
  );
}
