import { useEffect, useRef, useState } from "react";
import { z } from "zod";
import { DictationButton, appendDictation } from "./dictation";
import { onVisible } from "./visibility";
import {
  catalogQueryFilters,
  catalogQueryParams,
  metadataCatalogRow,
  parseCatalogQuery,
  queryCatalog,
  type CatalogQuery,
  type CatalogSort,
} from "../catalog/catalog-query";
import {
  capabilityLabels,
  capabilitySchema,
  type Capability,
} from "../catalog/capabilities";
import { catalogPanelKeys, detailsUrl, readDetails } from "./navigation";
import { ModelDetails } from "./model-details";
import { ModelDiscovery } from "./model-discovery";
import {
  modelManagementSchema,
  type ModelManagementAction,
} from "../domains/models/model-management-contract";
import {
  api,
  catalogModels,
  modelMemoryRequirement,
  modelName,
  modes,
  SessionRequiredError,
  type Connection,
  type Model,
} from "./client";

const responseSchema = modelManagementSchema.extend({ canManage: z.boolean() });
type ManagementState = z.infer<typeof responseSchema>;
type LocalModel = ManagementState["models"][number];
const modeNames = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcription",
  video: "Video",
  embedding: "Embeddings",
  decision: "Decision",
};
const fitOptions = [
  ["fits,tight", "Fits this machine"],
  ["fits", "Fits comfortably"],
  ["tight", "Tight fit"],
  ["wont-fit", "Won't fit"],
] as const;
const licenseOptions = [
  ["open", "Commercial use OK"],
  ["conditional", "Commercial use with conditions"],
  ["prohibited", "Non-commercial only"],
] as const;
const sortOptions: [CatalogSort, string][] = [
  ["name", "Name"],
  ["size", "Download size"],
  ["memory", "Memory needed"],
  ["fit", "Fit on this machine"],
  ["context", "Context length"],
];
const fitBadges = {
  fits: "Fits",
  tight: "Tight fit",
  "wont-fit": "Won't fit",
} as const;

/** The catalog filters: the shared query plus the UI-only enabled filter. */
type Filters = { query: CatalogQuery; enabled: boolean };

function readFilters(): Filters {
  const params = new URLSearchParams(location.search);
  return {
    query: parseCatalogQuery(params, modes),
    enabled: params.get("enabled") === "true",
  };
}

/** Mirrors the filters into the URL without adding history entries. */
function writeFilters({ query, enabled }: Filters) {
  const url = new URL(location.href);
  const details = url.searchParams.get("details");
  for (const key of catalogPanelKeys) url.searchParams.delete(key);
  for (const [key, value] of catalogQueryParams(query))
    url.searchParams.append(key, value);
  if (enabled) url.searchParams.set("enabled", "true");
  // An open details view survives filter changes made behind it.
  if (details) url.searchParams.set("details", details);
  const next = url.pathname + url.search;
  if (next !== location.pathname + location.search)
    history.replaceState(history.state, "", next);
}

/** Embedding and decision models are LLM-kind entries, listed under their own mode. */
function modelMode(model: Model) {
  const kind = model.catalog.capabilities?.kind;
  return kind === "embedding" || kind === "decision"
    ? kind
    : model.catalog.kind;
}

/** A select value for a list filter, or "" when it is unset. */
const listValue = (values: readonly string[] | undefined) =>
  values?.join(",") ?? "";
/** Splits a select value back into a list filter, unset for "". */
const listFilter = <T extends string>(value: string) =>
  value ? (value.split(",") as T[]) : undefined;

/** An extra option so a hand-edited URL's combination still shows. */
function customOption(
  value: string,
  options: readonly (readonly [string, string])[],
) {
  return value && !options.some(([option]) => option === value) ? (
    <option value={value}>{value.replaceAll(",", ", ")}</option>
  ) : null;
}

function bytes(value: number | null) {
  if (value === null) return "Unknown";
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(0)} KiB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MiB`;
  return `${(value / 1024 ** 3).toFixed(1)} GiB`;
}

/** The storage line: free space, or why it is not known yet. */
export function storageSummary(
  storage: ManagementState["storage"] | null,
  failed: boolean,
) {
  if (storage)
    return `${bytes(storage.availableBytes)} free of ${bytes(storage.totalBytes)}`;
  // A failed status check must not look like one that is still running.
  return failed ? "Storage unavailable" : "Checking storage…";
}

export function ModelManagement({
  connection,
  models,
  refreshModels,
  onSessionExpired,
  openSettings,
  canDictate,
}: {
  connection: Connection | null;
  models: Model[];
  refreshModels: (signal?: AbortSignal) => Promise<void>;
  onSessionExpired: (error: SessionRequiredError) => void;
  openSettings: () => void;
  canDictate: boolean;
}) {
  const [state, setState] = useState<ManagementState | null>(null);
  const [filters, setFilters] = useState(readFilters);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const [error, setError] = useState("");
  // Why the latest status poll failed; cleared when a poll succeeds.
  const [loadError, setLoadError] = useState("");
  const [pending, setPending] = useState(false);
  const [confirmation, setConfirmation] = useState<{
    modelId: string;
    action: "install" | "uninstall";
  } | null>(null);
  const [revision, setRevision] = useState(0);
  const [view, setView] = useState<"catalog" | "discover">("catalog");
  const [details, setDetails] = useState(() => readDetails(location.search));
  // True while the open details view is a history entry this page pushed.
  const pushedDetails = useRef(false);
  const connectionKind = connection?.kind;

  useEffect(() => {
    setState(null);
    setConfirmation(null);
    if (!connection) return;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let polling = false;
    const poll = async () => {
      clearTimeout(timer);
      timer = undefined;
      polling = true;
      let running = false;
      try {
        const next = responseSchema.parse(
          await (
            await api("/_localbase/model-management", connection, {
              signal: abort.signal,
            })
          ).json(),
        );
        if (abort.signal.aborted) return;
        setState(next);
        setLoadError("");
        running = next.models.some(
          (model) => model.operation?.state === "running",
        );
        await refreshModels(abort.signal);
      } catch (error) {
        if (!abort.signal.aborted) {
          setState(null);
          if (error instanceof SessionRequiredError) onSessionExpired(error);
          setLoadError(
            error instanceof Error
              ? error.message
              : "Could not load model status.",
          );
        }
      } finally {
        polling = false;
        // A hidden tab stops polling until it is shown again.
        if (!abort.signal.aborted && !document.hidden)
          timer = setTimeout(() => void poll(), running ? 1500 : 10000);
      }
    };
    void poll();
    const stopResume = onVisible(() => {
      if (!polling) void poll();
    });
    return () => {
      abort.abort();
      clearTimeout(timer);
      stopResume();
    };
  }, [connectionKind, revision]);

  async function run(modelId: string, action: ModelManagementAction) {
    if (!connection || !state?.canManage || pending) return;
    setPending(true);
    setError("");
    try {
      const response = await api("/_localbase/model-management", connection, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ modelId, action }),
      });
      await response.body?.cancel();
      setConfirmation(null);
      setRevision((value) => value + 1);
    } catch (error) {
      if (error instanceof SessionRequiredError) onSessionExpired(error);
      setError(error instanceof Error ? error.message : "Model update failed.");
    } finally {
      setPending(false);
    }
  }

  useEffect(() => {
    // Back and Forward can change the query while the catalog stays open.
    const restore = () => {
      setFilters(readFilters());
      setDetails(readDetails(location.search));
      pushedDetails.current = false;
    };
    window.addEventListener("popstate", restore);
    return () => window.removeEventListener("popstate", restore);
  }, []);
  function openDetails(modelId: string) {
    history.pushState(history.state, "", detailsUrl(location.href, modelId));
    pushedDetails.current = true;
    setDetails(modelId);
  }
  function closeDetails() {
    setDetails(null);
    // Closing undoes the entry opening added, so Back does not reopen it.
    if (pushedDetails.current) {
      pushedDetails.current = false;
      history.back();
    } else
      history.replaceState(history.state, "", detailsUrl(location.href, null));
  }
  function updateFilters(change: Partial<Filters>) {
    setFilters((current) => {
      const next = { ...current, ...change };
      writeFilters(next);
      return next;
    });
  }
  function updateQuery(change: Partial<CatalogQuery>) {
    setFilters((current) => {
      const query = { ...current.query, ...change };
      for (const key of Object.keys(query) as (keyof CatalogQuery)[])
        if (query[key] === undefined) delete query[key];
      const next = { ...current, query };
      writeFilters(next);
      return next;
    });
  }
  const { query, enabled } = filters;
  const filtering = catalogQueryFilters(query) || enabled;
  const onlyMode = query.kinds?.length === 1 ? query.kinds[0] : undefined;
  const selectedMode = modes.find((value) => value === onlyMode);
  // One mode keeps that mode's usual order: enabled, then installed, first.
  const filtered = queryCatalog(
    (selectedMode ? catalogModels(models, selectedMode) : models)
      .filter(
        (model) =>
          model.catalog.qualificationState !== "experimental" ||
          state?.models.some((entry) => entry.id === model.id),
      )
      .filter((model) => !enabled || model.device.selected),
    (model) => ({ ...metadataCatalogRow(model), kind: modelMode(model) }),
    query,
  );
  const installation = enabled
    ? "enabled"
    : query.installed === undefined
      ? "all"
      : query.installed
        ? "installed"
        : "uninstalled";
  const kindValue = listValue(query.kinds);
  const fitValue = listValue(query.fits);
  const licenseValue = listValue(query.licenses);
  const selectedCapabilities = query.capabilities ?? [];
  const running =
    state?.models.some((model) => model.operation?.state === "running") ??
    false;
  const locked = pending || running || !state?.canManage;
  const detailsModel = details
    ? models.find((model) => model.id === details)
    : undefined;

  /** Progress, actions, and confirmations; a card and its details share them. */
  function controls(
    model: Model,
    local: LocalModel | undefined,
    compact = false,
  ) {
    const operation = local?.operation;
    return (
      <>
        {operation && (
          <div className={`model-operation ${operation.state}`} role="status">
            <span>{operation.detail}</span>
            {operation.state === "running" && (
              <progress
                aria-label={`Progress for ${modelName(model)}`}
                max={operation.totalBytes ?? undefined}
                value={
                  operation.totalBytes && operation.downloadedBytes !== null
                    ? operation.downloadedBytes
                    : undefined
                }
              />
            )}
          </div>
        )}
        <div className="catalog-actions">
          {model.device.selected && !model.device.installed && (
            <button
              disabled={locked}
              onClick={() => void run(model.id, "disable")}
            >
              Disable
            </button>
          )}
          {!model.device.installed && (local?.installedBytes ?? 0) > 0 && (
            <button
              className="danger"
              disabled={
                locked ||
                model.device.selected ||
                Boolean(model.device.runtime?.configured)
              }
              onClick={() =>
                setConfirmation({
                  modelId: model.id,
                  action: "uninstall",
                })
              }
            >
              Uninstall
            </button>
          )}
          {model.device.installed ? (
            <>
              <button
                className={model.device.selected ? undefined : "install-model"}
                disabled={locked}
                onClick={() =>
                  void run(
                    model.id,
                    model.device.selected ? "disable" : "enable",
                  )
                }
              >
                {model.device.selected ? "Disable" : "Enable"}
              </button>
              {!compact && (
                <>
                  <button
                    disabled={locked || !model.device.selected || local?.active}
                    onClick={() => void run(model.id, "activate")}
                  >
                    {local?.active ? "Default model" : "Use by default"}
                  </button>
                  <button
                    className="danger"
                    disabled={
                      locked ||
                      model.device.selected ||
                      Boolean(model.device.runtime?.configured)
                    }
                    onClick={() =>
                      setConfirmation({
                        modelId: model.id,
                        action: "uninstall",
                      })
                    }
                  >
                    Uninstall
                  </button>
                </>
              )}
            </>
          ) : (
            <button
              className="install-model"
              disabled={locked || !local?.canInstall}
              onClick={() =>
                model.catalog.qualificationState === "experimental"
                  ? setConfirmation({
                      modelId: model.id,
                      action: "install",
                    })
                  : void run(model.id, "install")
              }
            >
              {operation?.state === "running" ? "Installing…" : "Install"}
            </button>
          )}
        </div>
        {!model.device.installed && local && !local.canInstall && (
          <p className="hint">{local.installUnavailableReason}</p>
        )}
        {!compact &&
          model.device.installed &&
          (model.device.selected || model.device.runtime?.configured) && (
            <p className="hint">
              Disable this model and wait for its runtime to stop before
              uninstalling.
            </p>
          )}
        {confirmation?.modelId === model.id && (
          <div
            className="uninstall-confirmation"
            role="group"
            aria-label={
              confirmation.action === "install"
                ? `Confirm experimental install ${model.id}`
                : `Confirm uninstall ${model.id}`
            }
          >
            {confirmation.action === "install" ? (
              <>
                <p>
                  This unqualified experimental model downloads about{" "}
                  {model.catalog.memory.storageEstimateGb} GB. Its behavior and
                  resource estimates have not been hardware qualified.
                </p>
                <button
                  disabled={locked || !local?.canInstall}
                  onClick={() => void run(model.id, "install")}
                >
                  Acknowledge and install
                </button>
              </>
            ) : (
              <>
                <p>
                  Remove {modelName(model)}'s downloaded files from this
                  computer? Shared files will be kept. Reinstalling requires
                  another download.
                </p>
                <button
                  disabled={locked}
                  className="danger"
                  onClick={() => void run(model.id, "uninstall")}
                >
                  Confirm uninstall
                </button>
              </>
            )}
            <button onClick={() => setConfirmation(null)}>
              {confirmation.action === "install" ? "Keep model" : "Cancel"}
            </button>
          </div>
        )}
      </>
    );
  }
  return (
    <div className="model-management">
      {!connection ? (
        <p className="notice">
          Connect to your gateway to browse its catalog.{" "}
          <button onClick={openSettings}>Connect</button>
        </p>
      ) : (
        <>
          <div className="admin-tabs catalog-views" aria-label="Catalog view">
            <button
              type="button"
              aria-pressed={view === "catalog"}
              onClick={() => setView("catalog")}
            >
              Local catalog
            </button>
            <button
              type="button"
              aria-pressed={view === "discover"}
              onClick={() => setView("discover")}
            >
              Find more models
            </button>
          </div>
          {view === "discover" ? (
            <ModelDiscovery
              connection={connection}
              models={models}
              openCatalog={(id) => {
                setView("catalog");
                openDetails(id);
              }}
              canManage={Boolean(state?.canManage)}
              onAdded={() => setRevision((value) => value + 1)}
            />
          ) : (
            <>
              <div className="storage-summary">
                <span>
                  <strong>
                    {models.filter((model) => model.device.installed).length}
                  </strong>{" "}
                  installed{" "}
                  <span className="muted">/ {models.length} models</span>
                </span>
                <span>
                  {storageSummary(state?.storage ?? null, Boolean(loadError))}
                </span>
              </div>
              {state && !state.canManage && (
                <p className="notice" role="status">
                  Read-only access. Sign in with an account authorized to manage
                  models.
                </p>
              )}
              <div
                className="catalog-filters"
                role="search"
                aria-label="Catalog filters"
              >
                <div className="dictation-field catalog-search">
                  <input
                    type="search"
                    aria-label="Search catalog"
                    placeholder="Search models…"
                    value={query.search ?? ""}
                    onChange={(event) =>
                      updateQuery({ search: event.target.value || undefined })
                    }
                  />
                  {canDictate && (
                    <DictationButton
                      label="catalog search"
                      onText={(text) =>
                        updateQuery({
                          search: appendDictation(query.search ?? "", text),
                        })
                      }
                    />
                  )}
                </div>
              </div>
              <div className="catalog-filter-bar">
                <button
                  type="button"
                  className="catalog-filter-toggle"
                  aria-expanded={filtersOpen}
                  aria-controls="catalog-filter-fields"
                  onClick={() => setFiltersOpen(!filtersOpen)}
                >
                  <span aria-hidden="true">{filtersOpen ? "▾" : "▸"} </span>
                  Filters & sort
                  {filtering
                    ? ` · ${[query.search, query.kinds?.length, query.fits?.length, query.licenses?.length, query.capabilities?.length, query.installed !== undefined || enabled].filter(Boolean).length} active`
                    : ""}
                </button>
                <button
                  className="catalog-installed"
                  aria-pressed={query.installed === true && !enabled}
                  onClick={() =>
                    updateFilters({
                      enabled: false,
                      query: {
                        ...query,
                        installed:
                          query.installed === true && !enabled
                            ? undefined
                            : true,
                      },
                    })
                  }
                >
                  Installed
                </button>
                {filtering && (
                  <button
                    className="catalog-clear"
                    onClick={() => updateFilters({ enabled: false, query: {} })}
                  >
                    Clear filters
                  </button>
                )}
              </div>
              <div
                className="catalog-filter-fields"
                id="catalog-filter-fields"
                hidden={!filtersOpen}
              >
                <label>
                  Mode
                  <select
                    aria-label="Filter model mode"
                    value={kindValue}
                    onChange={(event) =>
                      updateQuery({ kinds: listFilter(event.target.value) })
                    }
                  >
                    <option value="">All modes</option>
                    {modes.map((mode) => (
                      <option value={mode} key={mode}>
                        {modeNames[mode]}
                      </option>
                    ))}
                    {customOption(
                      kindValue,
                      modes.map((mode) => [mode, modeNames[mode]] as const),
                    )}
                  </select>
                </label>
                <label>
                  Availability
                  <select
                    aria-label="Filter installation"
                    value={installation}
                    onChange={(event) => {
                      const value = event.target.value;
                      updateFilters({
                        enabled: value === "enabled",
                        query: {
                          ...query,
                          installed:
                            value === "installed"
                              ? true
                              : value === "uninstalled"
                                ? false
                                : undefined,
                        },
                      });
                    }}
                  >
                    <option value="all">All models</option>
                    <option value="installed">Installed</option>
                    <option value="uninstalled">Not installed</option>
                    <option value="enabled">Enabled</option>
                  </select>
                </label>
                <label>
                  Memory fit
                  <select
                    aria-label="Filter by fit on this machine"
                    value={fitValue}
                    onChange={(event) =>
                      updateQuery({ fits: listFilter(event.target.value) })
                    }
                  >
                    <option value="">Any fit</option>
                    {fitOptions.map(([value, label]) => (
                      <option value={value} key={value}>
                        {label}
                      </option>
                    ))}
                    {customOption(fitValue, fitOptions)}
                  </select>
                </label>
                <label>
                  License
                  <select
                    aria-label="Filter by license"
                    value={licenseValue}
                    onChange={(event) =>
                      updateQuery({
                        licenses: listFilter(event.target.value),
                      })
                    }
                  >
                    <option value="">Any license</option>
                    {licenseOptions.map(([value, label]) => (
                      <option value={value} key={value}>
                        {label}
                      </option>
                    ))}
                    {customOption(licenseValue, licenseOptions)}
                  </select>
                </label>
                <label>
                  Capability
                  <select
                    aria-label="Add a capability filter"
                    value=""
                    onChange={(event) => {
                      const capability = capabilitySchema.safeParse(
                        event.target.value,
                      );
                      if (capability.success)
                        updateQuery({
                          capabilities: [
                            ...selectedCapabilities,
                            capability.data,
                          ],
                        });
                    }}
                  >
                    <option value="">
                      {selectedCapabilities.length
                        ? "Add capability…"
                        : "Any capability"}
                    </option>
                    {capabilitySchema.options
                      .filter((id) => !selectedCapabilities.includes(id))
                      .map((id) => (
                        <option value={id} key={id}>
                          {capabilityLabels[id]}
                        </option>
                      ))}
                  </select>
                </label>
                <div className="catalog-sort-field">
                  <label htmlFor="catalog-sort">Sort by</label>
                  <div className="catalog-sort">
                    <select
                      aria-label="Sort models"
                      id="catalog-sort"
                      value={query.sort ?? ""}
                      onChange={(event) =>
                        updateQuery({
                          sort: (event.target.value || undefined) as
                            CatalogSort | undefined,
                        })
                      }
                    >
                      <option value="">Sort: default order</option>
                      {sortOptions.map(([value, label]) => (
                        <option value={value} key={value}>
                          {label}
                        </option>
                      ))}
                    </select>
                    <button
                      type="button"
                      className="catalog-reverse"
                      aria-pressed={Boolean(query.reverse)}
                      aria-label="Reverse order"
                      title="Reverse order"
                      onClick={() =>
                        updateQuery({
                          reverse: query.reverse ? undefined : true,
                        })
                      }
                    >
                      <span aria-hidden="true">⇅</span>
                    </button>
                  </div>
                </div>
              </div>
              {selectedCapabilities.length > 0 && (
                <ul
                  className="catalog-chips"
                  aria-label="Required capabilities"
                >
                  {selectedCapabilities.map((id) => (
                    <li key={id}>
                      <button
                        type="button"
                        className="catalog-chip"
                        aria-label={`Remove ${capabilityLabels[id]} filter`}
                        onClick={() =>
                          updateQuery({
                            capabilities: listFilter<Capability>(
                              selectedCapabilities
                                .filter((value) => value !== id)
                                .join(","),
                            ),
                          })
                        }
                      >
                        {capabilityLabels[id]} <span aria-hidden="true">×</span>
                      </button>
                    </li>
                  ))}
                </ul>
              )}
              {(error || loadError) && (
                <div className="error" role="alert">
                  <span>{error || loadError}</span>
                  <button
                    onClick={() => {
                      setError("");
                      setLoadError("");
                      setRevision((value) => value + 1);
                    }}
                  >
                    Refresh
                  </button>
                </div>
              )}
              <p className="catalog-count" role="status">
                {filtered.length === 1
                  ? "1 model"
                  : `${filtered.length} models`}
                {filtering
                  ? filtered.length === 1
                    ? " matches"
                    : " match"
                  : ""}
              </p>
              <div className="catalog-grid">
                {filtered.map((model) => {
                  const local = state?.models.find(
                    (item) => item.id === model.id,
                  );
                  return (
                    <article className="catalog-card" key={model.id}>
                      <header>
                        <h3>{modelName(model)}</h3>
                        {model.catalog.qualificationState ===
                          "experimental" && (
                          <span className="model-badge">Experimental</span>
                        )}
                        <span
                          className={`model-badge ${model.device.installed ? "installed" : ""}`}
                        >
                          {model.device.installed
                            ? "Installed"
                            : "Not installed"}
                        </span>
                        {model.catalog.memory.fit && (
                          <span
                            className={`model-badge fit-${model.catalog.memory.fit.verdict}`}
                          >
                            {fitBadges[model.catalog.memory.fit.verdict]}
                          </span>
                        )}
                      </header>
                      <p className="model-facts">
                        {model.catalog.quantization} ·{" "}
                        {model.device.selected ? "Enabled" : "Disabled"}
                        {local?.active ? " · Default" : ""}
                      </p>
                      <dl className="model-storage catalog-card-storage">
                        <div>
                          <dt>Download size</dt>
                          <dd>
                            {local?.downloadBytes != null
                              ? bytes(local.downloadBytes)
                              : `~${model.catalog.memory.storageEstimateGb} GB`}
                          </dd>
                        </div>
                        <div>
                          <dt>Min. memory</dt>
                          <dd>{modelMemoryRequirement(model)}</dd>
                        </div>
                      </dl>
                      <div className="catalog-card-footer">
                        {controls(model, local, true)}
                        <button
                          type="button"
                          className="catalog-details-button"
                          aria-label={`Details for ${modelName(model)}`}
                          onClick={() => openDetails(model.id)}
                        >
                          Details
                        </button>
                      </div>
                    </article>
                  );
                })}
              </div>
              {!filtered.length && (
                <div className="catalog-empty" role="status">
                  <p>No models match these filters.</p>
                  {filtering && (
                    <button
                      type="button"
                      onClick={() =>
                        updateFilters({
                          enabled: false,
                          query: {
                            ...(query.sort ? { sort: query.sort } : {}),
                            ...(query.reverse ? { reverse: true } : {}),
                          },
                        })
                      }
                    >
                      Clear filters
                    </button>
                  )}
                </div>
              )}
              {detailsModel && (
                <ModelDetails
                  key={detailsModel.id}
                  model={detailsModel}
                  status={state?.models.find(
                    (item) => item.id === detailsModel.id,
                  )}
                  close={closeDetails}
                  actions={controls(
                    detailsModel,
                    state?.models.find((item) => item.id === detailsModel.id),
                  )}
                />
              )}
              <details className="catalog-help">
                <summary>How model setup works</summary>
                <p className="hint">
                  Install downloads the files. Enable makes a model available
                  for requests. Set a default in model details for clients that
                  do not choose a model.
                </p>
              </details>
              <p className="hint">
                File sizes can include artifacts shared by multiple models.
                Memory requirements are estimates, not reserved memory.
              </p>
            </>
          )}
        </>
      )}
    </div>
  );
}
