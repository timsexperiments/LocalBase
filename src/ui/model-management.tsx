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
  capabilityLabel,
  capabilityLabels,
  capabilitySourceLabels,
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
  modelFitLabel,
  modelMemoryRequirement,
  modelName,
  modes,
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

const fitSources = {
  estimate: "Based on an estimate",
  measured: "Based on a measurement",
  feed: "Based on a published measurement",
  observed: "Based on use here",
} as const;

/** The fit verdict and what memory figure it was judged from. */
function fitDetail(model: Model) {
  const label = modelFitLabel(model);
  const source = model.catalog.memory.source;
  return (
    <>
      {label}
      {source && <span className="fit-source">{fitSources[source]}</span>}
    </>
  );
}

/** Effective capabilities; runtime and detected ones say so on hover. */
function CapabilityBadges({ model }: { model: Model }) {
  const capabilities =
    model.catalog.capabilityResolution?.capabilities ??
    model.catalog.features.map((id) => ({ id, source: "declared" }));
  if (!capabilities.length) return null;
  return (
    <ul className="capability-badges" aria-label="Capabilities">
      {capabilities.map(({ id, source }) => (
        <li
          key={id}
          className={`model-badge capability ${source}`}
          title={
            source === "declared"
              ? undefined
              : `${capabilityLabel(id)}: ${capabilitySourceLabels[source as keyof typeof capabilitySourceLabels] ?? source}`
          }
        >
          {capabilityLabel(id)}
        </li>
      ))}
    </ul>
  );
}

function bytes(value: number | null) {
  if (value === null) return "Unknown";
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(0)} KiB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MiB`;
  return `${(value / 1024 ** 3).toFixed(1)} GiB`;
}

export function ModelManagement({
  connection,
  models,
  refreshModels,
  openSettings,
  canDictate,
}: {
  connection: Connection | null;
  models: Model[];
  refreshModels: (signal?: AbortSignal) => Promise<void>;
  openSettings: () => void;
  canDictate: boolean;
}) {
  const [state, setState] = useState<ManagementState | null>(null);
  const [filters, setFilters] = useState(readFilters);
  const [error, setError] = useState("");
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
        running = next.models.some(
          (model) => model.operation?.state === "running",
        );
        await refreshModels(abort.signal);
      } catch (error) {
        if (!abort.signal.aborted) {
          setState(null);
          setError(
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
  function controls(model: Model, local: LocalModel | undefined) {
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
        {model.device.installed &&
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
                  {state
                    ? `${bytes(state.storage.availableBytes)} free of ${bytes(state.storage.totalBytes)}`
                    : "Checking storage…"}
                </span>
              </div>
              <p className="hint">
                Install downloads the files. Enable makes a model available for
                requests. The default is used when a client does not choose a
                model.
              </p>
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
                    placeholder="Search names, ids, and descriptions…"
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
                <select
                  aria-label="Filter by license"
                  value={licenseValue}
                  onChange={(event) =>
                    updateQuery({ licenses: listFilter(event.target.value) })
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
                <div className="catalog-sort">
                  <select
                    aria-label="Sort models"
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
                      updateQuery({ reverse: query.reverse ? undefined : true })
                    }
                  >
                    <span aria-hidden="true">⇅</span>
                  </button>
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
              {error && (
                <div className="error" role="alert">
                  <span>{error}</span>
                  <button
                    onClick={() => {
                      setError("");
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
                      <p className="model-id">{model.id}</p>
                      {model.catalog.qualificationState === "experimental" && (
                        <p className="hint">
                          Unqualified; hardware behavior and admission estimates
                          need validation.
                        </p>
                      )}
                      <p className="model-facts">
                        {model.catalog.quantization} ·{" "}
                        {model.device.selected ? "Enabled" : "Disabled"}
                        {local?.active ? " · Default" : ""}
                      </p>
                      {model.catalog.summary && (
                        <p className="model-summary">{model.catalog.summary}</p>
                      )}
                      <CapabilityBadges model={model} />
                      <dl className="model-storage">
                        <div>
                          <dt>Model files</dt>
                          <dd>
                            {local ? bytes(local.installedBytes) : "Checking…"}
                          </dd>
                        </div>
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
                        {modelFitLabel(model) && (
                          <div className="model-storage-fit">
                            <dt>This machine</dt>
                            <dd>{fitDetail(model)}</dd>
                          </div>
                        )}
                      </dl>
                      {controls(model, local)}
                      <button
                        type="button"
                        className="catalog-details-button"
                        aria-label={`Details for ${modelName(model)}`}
                        onClick={() => openDetails(model.id)}
                      >
                        Details
                      </button>
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
