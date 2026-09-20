import { useEffect, useState } from "react";
import { z } from "zod";
import { DictationButton, appendDictation } from "./dictation";
import {
  modelManagementSchema,
  type ModelManagementAction,
} from "../domains/models/model-management-contract";
import {
  api,
  catalogModels,
  modelMemoryRequirement,
  modes,
  type Connection,
  type HostMemory,
  type Model,
} from "./client";

const responseSchema = modelManagementSchema.extend({ canManage: z.boolean() });
type ManagementState = z.infer<typeof responseSchema>;
const modeNames = {
  llm: "Chat",
  image: "Image",
  tts: "Speech",
  stt: "Transcription",
  video: "Video",
  embedding: "Embeddings",
};
function bytes(value: number | null) {
  if (value === null) return "Unknown";
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(0)} KiB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MiB`;
  return `${(value / 1024 ** 3).toFixed(1)} GiB`;
}

export function ModelManagement({
  connection,
  models,
  hostMemory,
  refreshModels,
  openSettings,
}: {
  connection: Connection | null;
  models: Model[];
  hostMemory: HostMemory | null;
  refreshModels: (signal?: AbortSignal) => Promise<void>;
  openSettings: () => void;
}) {
  const [state, setState] = useState<ManagementState | null>(null);
  const [search, setSearch] = useState("");
  const [mode, setMode] = useState("all");
  const [installation, setInstallation] = useState("all");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [confirmation, setConfirmation] = useState<string | null>(null);
  const [revision, setRevision] = useState(0);
  const connectionKind = connection?.kind;
  const key = connection?.kind === "api-key" ? connection.key : "";

  useEffect(() => {
    setState(null);
    setConfirmation(null);
    if (!connection) return;
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
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
        if (!abort.signal.aborted)
          timer = setTimeout(() => void poll(), running ? 1500 : 10000);
      }
    };
    void poll();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  }, [connectionKind, key, revision]);

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

  const selectedMode = modes.find((value) => value === mode);
  const filtered = (selectedMode ? catalogModels(models, selectedMode) : models)
    .filter((model) =>
      `${model.id} ${model.catalog.name} ${model.catalog.quantization}`
        .toLowerCase()
        .includes(search.toLowerCase()),
    )
    .filter(
      (model) =>
        installation === "all" ||
        (installation === "installed"
          ? model.device.installed
          : installation === "enabled"
            ? model.device.selected
            : !model.device.installed),
    );
  const running =
    state?.models.some((model) => model.operation?.state === "running") ??
    false;
  const locked = pending || running || !state?.canManage;

  return (
    <div className="model-management">
      {!connection ? (
        <p className="notice">
          Connect to your gateway to browse its catalog.{" "}
          <button onClick={openSettings}>Connect</button>
        </p>
      ) : (
        <>
          <div className="storage-summary">
            <span>
              <strong>
                {models.filter((model) => model.device.installed).length}
              </strong>{" "}
              installed <span className="muted">/ {models.length} models</span>
            </span>
            <span>
              {state
                ? `Disk storage: ${bytes(state.storage.availableBytes)} available of ${bytes(state.storage.totalBytes)}`
                : "Checking disk storage…"}
            </span>
          </div>
          <p className="hint">
            Install downloads the files. Enable makes a model available for
            requests. The default is used when a client does not choose a model.
          </p>
          {state && !state.canManage && (
            <p className="notice" role="status">
              Read-only access. Use your personal sign-in or an authorized
              management key to change models.
            </p>
          )}
          <div className="catalog-filters">
            <div className="dictation-field catalog-search">
              <input
                type="search"
                aria-label="Search catalog"
                placeholder="Search the full catalog…"
                value={search}
                onChange={(event) => setSearch(event.target.value)}
              />
              <DictationButton
                label="catalog search"
                onText={(text) =>
                  setSearch((value) => appendDictation(value, text))
                }
              />
            </div>
            <select
              aria-label="Filter model mode"
              value={mode}
              onChange={(event) => setMode(event.target.value)}
            >
              <option value="all">All modes</option>
              {modes.map((mode) => (
                <option value={mode} key={mode}>
                  {modeNames[mode]}
                </option>
              ))}
            </select>
            <select
              aria-label="Filter installation"
              value={installation}
              onChange={(event) => setInstallation(event.target.value)}
            >
              <option value="all">All models</option>
              <option value="installed">Installed</option>
              <option value="uninstalled">Not installed</option>
              <option value="enabled">Enabled</option>
            </select>
          </div>
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
            {filtered.length} models
          </p>
          <div className="catalog-grid">
            {filtered.map((model) => {
              const local = state?.models.find((item) => item.id === model.id);
              const operation = local?.operation;
              const memory = modelMemoryRequirement(model, hostMemory);
              return (
                <article className="catalog-card" key={model.id}>
                  <header>
                    <h3>{model.catalog.name}</h3>
                    <span
                      className={`model-badge ${model.device.installed ? "installed" : ""}`}
                    >
                      {model.device.installed ? "Installed" : "Not installed"}
                    </span>
                  </header>
                  <p className="model-id">{model.id}</p>
                  <p className="model-facts">
                    {model.catalog.quantization} ·{" "}
                    {model.device.selected ? "Enabled" : "Disabled"}
                    {local?.active ? " · Default" : ""}
                  </p>
                  <dl className="model-storage">
                    <div>
                      <dt>Model files</dt>
                      <dd>
                        {local ? bytes(local.installedBytes) : "Checking…"}
                      </dd>
                    </div>
                    <div>
                      <dt>Expected download</dt>
                      <dd>
                        {local?.remainingDownloadBytes != null
                          ? bytes(local.remainingDownloadBytes)
                          : `~${model.catalog.memory.storageEstimateGb} GB`}
                      </dd>
                    </div>
                    <div>
                      <dt>{memory.label}</dt>
                      <dd>
                        {memory.gigabytes === null
                          ? "Not measured"
                          : `${memory.gigabytes} GiB`}
                      </dd>
                    </div>
                  </dl>
                  {operation && (
                    <div
                      className={`model-operation ${operation.state}`}
                      role="status"
                    >
                      <span>{operation.detail}</span>
                      {operation.state === "running" && (
                        <progress
                          aria-label={`Progress for ${model.catalog.name}`}
                          max={operation.totalBytes ?? undefined}
                          value={
                            operation.totalBytes &&
                            operation.downloadedBytes !== null
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
                    {!model.device.installed &&
                      (local?.installedBytes ?? 0) > 0 && (
                        <button
                          className="danger"
                          disabled={
                            locked ||
                            model.device.selected ||
                            Boolean(model.device.runtime?.configured)
                          }
                          onClick={() => setConfirmation(model.id)}
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
                          disabled={
                            locked || !model.device.selected || local?.active
                          }
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
                          onClick={() => setConfirmation(model.id)}
                        >
                          Uninstall
                        </button>
                      </>
                    ) : (
                      <button
                        className="install-model"
                        disabled={locked || !local?.canInstall}
                        onClick={() => void run(model.id, "install")}
                      >
                        {operation?.state === "running"
                          ? "Installing…"
                          : "Install"}
                      </button>
                    )}
                  </div>
                  {!model.device.installed && local && !local.canInstall && (
                    <p className="hint">{local.installUnavailableReason}</p>
                  )}
                  {model.device.installed &&
                    (model.device.selected ||
                      model.device.runtime?.configured) && (
                      <p className="hint">
                        Disable this model and wait for its runtime to stop
                        before uninstalling.
                      </p>
                    )}
                  {confirmation === model.id && (
                    <div
                      className="uninstall-confirmation"
                      role="group"
                      aria-label={`Confirm uninstall ${model.id}`}
                    >
                      <p>
                        Remove {model.catalog.name}'s downloaded files from this
                        computer? Shared files will be kept. Reinstalling
                        requires another download.
                      </p>
                      <button
                        disabled={locked}
                        className="danger"
                        onClick={() => void run(model.id, "uninstall")}
                      >
                        Confirm uninstall
                      </button>
                      <button onClick={() => setConfirmation(null)}>
                        Keep model
                      </button>
                    </div>
                  )}
                </article>
              );
            })}
          </div>
          {!filtered.length && (
            <p className="hint">No models match these filters.</p>
          )}
          <p className="hint">
            File sizes can include artifacts shared by multiple models. Memory
            requirements are estimates, not reserved memory. Expected downloads
            reuse matching local files but verification may require a fresh
            download.
          </p>
        </>
      )}
    </div>
  );
}
