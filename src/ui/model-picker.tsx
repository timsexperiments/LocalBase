import {
  modelName,
  modelModalities,
  modelMemorySummary,
  type Model,
} from "./client";

export function ModelPickerList({
  models,
  selectedId,
  search,
  busy,
  hostMemory,
  onChoose,
}: {
  models: Model[];
  selectedId: string | undefined;
  search: string;
  busy: boolean;
  hostMemory: Parameters<typeof modelMemorySummary>[1];
  onChoose: (id: string) => void;
}) {
  const matching = models.filter((model) =>
    `${modelName(model)} ${model.catalog.name} ${model.id} ${model.catalog.quantization}`
      .toLowerCase()
      .includes(search.toLowerCase()),
  );
  const ready = matching.filter(
    (model) => model.device.installed && model.device.selected,
  );
  const unavailable = matching.filter(
    (model) => !model.device.installed || !model.device.selected,
  );
  function card(model: Model) {
    return (
      <button
        key={model.id}
        className={`model-card ${model.id === selectedId ? "selected" : ""}`}
        disabled={busy || !model.device.installed || !model.device.selected}
        aria-pressed={model.id === selectedId}
        onClick={() => onChoose(model.id)}
      >
        <strong>
          {modelName(model)}
          {model.id === selectedId && (
            <span className="picker-selected"> · Selected ✓</span>
          )}
        </strong>
        <span>
          {model.catalog.quantization} ·{" "}
          {model.device.installed
            ? model.device.selected
              ? "Ready to use"
              : "Enable in Manage models"
            : "Install in Manage models"}
        </span>
        <small>
          {modelModalities(model)}
          {model.catalog.contextWindowTokens
            ? ` · ${model.catalog.contextWindowTokens.toLocaleString()} context`
            : ""}
        </small>
        <small>{modelMemorySummary(model, hostMemory)}</small>
      </button>
    );
  }
  return (
    <div className="picker-results">
      <p className="catalog-count" role="status">
        {ready.length} ready to use
      </p>
      {ready.map(card)}
      {!ready.length && (
        <p className="hint">
          {matching.length
            ? "Install or enable a model in Manage models to start."
            : "No models match your search."}
        </p>
      )}
      {unavailable.length > 0 && (
        <details className="picker-unavailable">
          <summary>Available to set up · {unavailable.length}</summary>
          {unavailable.map(card)}
        </details>
      )}
    </div>
  );
}
