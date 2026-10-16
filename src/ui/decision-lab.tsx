import { useEffect, useRef, useState } from "react";
import {
  ApiError,
  SessionRequiredError,
  api,
  modelAcceptsInput,
  modelName,
  type Connection,
  type Model,
} from "./client";
import {
  blankQuestion,
  buildDecisionRequest,
  decisionBars,
  decisionConfidence,
  decisionCurl,
  decisionExamples,
  decisionHeadline,
  decisionQuestionTypeLabels,
  decisionQuestionTypes,
  decisionResponseSchema,
  draftsFromJson,
  exampleDrafts,
  formatLatency,
  formatPercent,
  looseQuestionsJson,
  maxDecisionImageBytes,
  maxDecisionImages,
  maxDecisionOptions,
  retypeQuestion,
  type DecisionAnswer,
  type DecisionQuestionDraft,
  type DecisionRequest,
  type DecisionResponse,
  type DecisionStateFormat,
} from "./decision";
import { Copy } from "./message-markdown";

type Image = { url: string; name: string };
type Run = {
  request: DecisionRequest;
  response: DecisionResponse;
  latencyMs: number;
};

const firstExample = decisionExamples[0]!;
const typeHints = {
  choice: "Picks one option and returns a probability for each.",
  score: "Rates on your levels and returns the expected level.",
  noul: "Returns the probability that the answer is yes.",
} as const;

function readDataUrl(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(new Error("Could not read that image."));
    reader.readAsDataURL(file);
  });
}

export function DecisionLab({
  model,
  credential,
  onChooseModel,
  onSessionExpired,
}: {
  model: Model | undefined;
  credential: Connection | null;
  onChooseModel: () => void;
  onSessionExpired: (message: string) => void;
}) {
  const [format, setFormat] = useState<DecisionStateFormat>(
    firstExample.format,
  );
  const [state, setState] = useState(firstExample.state);
  const [images, setImages] = useState<Image[]>([]);
  const [imageError, setImageError] = useState("");
  const [editor, setEditor] = useState<"builder" | "json">("builder");
  const [drafts, setDrafts] = useState(() => exampleDrafts(firstExample));
  const [questionsText, setQuestionsText] = useState("");
  const [editorError, setEditorError] = useState("");
  const [running, setRunning] = useState(false);
  const [error, setError] = useState("");
  const [run, setRun] = useState<Run | null>(null);
  const controller = useRef<AbortController | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const results = useRef<HTMLElement>(null);
  const acceptsImages = model ? modelAcceptsInput(model, "image") : false;
  useEffect(() => () => controller.current?.abort(), []);

  const built = buildDecisionRequest({
    model: model?.id ?? "",
    format,
    state,
    images: acceptsImages ? images.map((image) => image.url) : [],
    editor,
    drafts,
    questionsText,
  });

  function loadExample(id: string) {
    const example = decisionExamples.find((item) => item.id === id);
    if (!example) return;
    setFormat(example.format);
    setState(example.state);
    setDrafts(exampleDrafts(example));
    setQuestionsText(JSON.stringify(example.questions, null, 2));
    setEditorError("");
  }
  function switchEditor(next: "builder" | "json") {
    if (next === editor) return;
    if (next === "json") {
      setQuestionsText(looseQuestionsJson(drafts));
      setEditorError("");
      setEditor("json");
      return;
    }
    const parsed = draftsFromJson(questionsText);
    if (!parsed) {
      setEditorError(
        "The builder cannot show this JSON. Fix it, or keep editing it here.",
      );
      return;
    }
    setDrafts(parsed);
    setEditorError("");
    setEditor("builder");
  }
  function switchFormat(next: DecisionStateFormat) {
    if (next === format) return;
    if (next === "json" && state.trim()) {
      try {
        JSON.parse(state);
      } catch {
        setState(JSON.stringify(state));
      }
    }
    if (next === "text") {
      try {
        const value: unknown = JSON.parse(state);
        if (typeof value === "string") setState(value);
      } catch {
        // Keep the text as typed.
      }
    }
    setFormat(next);
  }
  const updateDraft = (
    uid: string,
    change: (draft: DecisionQuestionDraft) => DecisionQuestionDraft,
  ) =>
    setDrafts((items) =>
      items.map((item) => (item.uid === uid ? change(item) : item)),
    );

  async function addImages(files: FileList | File[]) {
    setImageError("");
    const chosen = [...files];
    if (images.length + chosen.length > maxDecisionImages) {
      setImageError(`Attach at most ${maxDecisionImages} images.`);
      return;
    }
    const next: Image[] = [];
    for (const file of chosen) {
      if (!file.type.startsWith("image/")) {
        setImageError(`${file.name} is not an image.`);
        return;
      }
      if (file.size > maxDecisionImageBytes) {
        setImageError(`${file.name} is larger than 10 MB.`);
        return;
      }
      try {
        next.push({ url: await readDataUrl(file), name: file.name });
      } catch (e) {
        setImageError(e instanceof Error ? e.message : String(e));
        return;
      }
    }
    setImages((items) => [...items, ...next]);
  }

  async function decide() {
    if (running || !credential || !model || !built.ok) return;
    const request = built.value;
    const abort = new AbortController();
    controller.current = abort;
    setRunning(true);
    setError("");
    const started = performance.now();
    try {
      const response = decisionResponseSchema.parse(
        await (
          await api("/v1/systemone", credential, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify(request),
            signal: abort.signal,
          })
        ).json(),
      );
      setRun({ request, response, latencyMs: performance.now() - started });
      if (matchMedia("(max-width: 900px)").matches)
        requestAnimationFrame(() =>
          results.current?.scrollIntoView({ block: "start" }),
        );
    } catch (e) {
      if (abort.signal.aborted) return;
      if (e instanceof SessionRequiredError) onSessionExpired(e.message);
      else
        setError(
          e instanceof ApiError || e instanceof Error
            ? e.message
            : "The decision failed. Try again.",
        );
    } finally {
      if (controller.current === abort) controller.current = null;
      setRunning(false);
    }
  }

  return (
    <div
      className="studio decision-lab"
      onKeyDown={(event) => {
        if (
          event.key === "Enter" &&
          (event.metaKey || event.ctrlKey) &&
          !event.nativeEvent.isComposing
        ) {
          event.preventDefault();
          void decide();
        }
      }}
    >
      <section className="decision-editor" aria-label="Decision inputs">
        <div className="decision-toolbar">
          <button
            type="button"
            className="model-picker studio-model"
            disabled={running}
            onClick={onChooseModel}
            aria-label={`Choose model: ${model ? modelName(model) : "none selected"}`}
          >
            <span
              className={`status-dot${model ? " ready" : ""}`}
              aria-hidden="true"
            />
            <span>{model ? modelName(model) : "Choose a decision model"}</span>
            <span aria-hidden="true">⌄</span>
          </button>
          <label className="decision-example">
            <span>Example</span>
            <select
              value=""
              disabled={running}
              onChange={(event) => loadExample(event.target.value)}
            >
              <option value="" disabled>
                Load an example…
              </option>
              {decisionExamples.map((example) => (
                <option key={example.id} value={example.id}>
                  {example.label}
                </option>
              ))}
            </select>
          </label>
        </div>
        {!model && (
          <p className="notice">
            No decision model is installed and enabled.{" "}
            <button type="button" className="link" onClick={onChooseModel}>
              Choose a model
            </button>{" "}
            or install Clef Flash with{" "}
            <code>local-base models install clef-flash-q4_k_m</code>.
          </p>
        )}

        <div className="studio-group">
          <div className="decision-heading">
            <span className="studio-heading" id="decision-state-label">
              State
            </span>
            <div
              className="studio-segmented"
              role="group"
              aria-label="State format"
            >
              {(["text", "json"] as const).map((value) => (
                <button
                  key={value}
                  type="button"
                  aria-pressed={format === value}
                  onClick={() => switchFormat(value)}
                >
                  {value === "text" ? "Text" : "JSON"}
                </button>
              ))}
            </div>
          </div>
          <textarea
            className={`studio-prompt decision-state${format === "json" ? " decision-code" : ""}`}
            aria-labelledby="decision-state-label"
            value={state}
            rows={format === "json" ? 8 : 4}
            spellCheck={format === "text"}
            placeholder={
              format === "text"
                ? "The message, record or situation to decide on"
                : '{"ticket": {"subject": "…"}}'
            }
            onChange={(event) => setState(event.target.value)}
          />
          {acceptsImages && (
            <>
              <input
                ref={fileInput}
                type="file"
                hidden
                multiple
                accept="image/png,image/jpeg,image/webp,image/gif"
                aria-label="Choose images"
                onChange={(event) => {
                  const files = event.currentTarget.files;
                  if (files) void addImages([...files]);
                  event.currentTarget.value = "";
                }}
              />
              {images.length > 0 && (
                <ul className="decision-images" aria-label="Images">
                  {images.map((image, index) => (
                    <li key={`${image.name}-${index}`}>
                      <img src={image.url} alt={image.name} />
                      <button
                        type="button"
                        aria-label={`Remove ${image.name}`}
                        onClick={() =>
                          setImages((items) =>
                            items.filter((_, n) => n !== index),
                          )
                        }
                      >
                        ×
                      </button>
                    </li>
                  ))}
                </ul>
              )}
              {images.length < maxDecisionImages && (
                <button
                  type="button"
                  className="studio-dropzone"
                  onClick={() => fileInput.current?.click()}
                  onDragOver={(event) => event.preventDefault()}
                  onDrop={(event) => {
                    event.preventDefault();
                    void addImages(event.dataTransfer.files);
                  }}
                >
                  <strong>Add images</strong>
                  <span>Drop or browse · optional</span>
                </button>
              )}
              {imageError && (
                <p className="generation-settings-hint" role="alert">
                  {imageError}
                </p>
              )}
            </>
          )}
        </div>

        <div className="studio-group">
          <div className="decision-heading">
            <span className="studio-heading">Questions</span>
            <div
              className="studio-segmented"
              role="group"
              aria-label="Question editor"
            >
              <button
                type="button"
                aria-pressed={editor === "builder"}
                onClick={() => switchEditor("builder")}
              >
                Builder
              </button>
              <button
                type="button"
                aria-pressed={editor === "json"}
                onClick={() => switchEditor("json")}
              >
                JSON
              </button>
            </div>
          </div>
          {editorError && (
            <p className="generation-settings-hint" role="alert">
              {editorError}
            </p>
          )}
          {editor === "json" ? (
            <textarea
              className="studio-prompt decision-code decision-questions-json"
              aria-label="Questions JSON"
              value={questionsText}
              rows={14}
              spellCheck={false}
              onChange={(event) => setQuestionsText(event.target.value)}
            />
          ) : (
            <>
              <ol className="decision-questions">
                {drafts.map((draft, index) => (
                  <QuestionEditor
                    key={draft.uid}
                    draft={draft}
                    index={index}
                    onChange={(change) => updateDraft(draft.uid, change)}
                    onRemove={
                      drafts.length > 1
                        ? () =>
                            setDrafts((items) =>
                              items.filter((item) => item.uid !== draft.uid),
                            )
                        : undefined
                    }
                  />
                ))}
              </ol>
              <div
                className="decision-add"
                role="group"
                aria-label="Add a question"
              >
                {decisionQuestionTypes.map((type) => (
                  <button
                    key={type}
                    type="button"
                    onClick={() =>
                      setDrafts((items) => [...items, blankQuestion(type)])
                    }
                  >
                    + {decisionQuestionTypeLabels[type]}
                  </button>
                ))}
              </div>
            </>
          )}
        </div>

        <div className="decision-actions">
          {!built.ok && (
            <p className="generation-settings-hint" role="status">
              {built.error}
            </p>
          )}
          {running ? (
            <button
              type="button"
              className="studio-generate"
              onClick={() => controller.current?.abort()}
            >
              Stop
            </button>
          ) : (
            <button
              type="button"
              className="studio-generate"
              disabled={!credential || !model || !built.ok}
              onClick={() => void decide()}
            >
              Decide
              <kbd>⌘↵</kbd>
            </button>
          )}
          {built.ok && (
            <Copy
              label="Copy as curl"
              className="decision-curl"
              text={() => decisionCurl(built.value, location.origin)}
            />
          )}
        </div>
      </section>

      <section
        ref={results}
        className="decision-results"
        aria-label="Decision results"
        aria-busy={running}
      >
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        {run ? (
          <>
            <div className="decision-meta">
              <span>
                <strong>{formatLatency(run.latencyMs)}</strong> round trip
              </span>
              {run.response.usage && (
                <span>
                  {run.response.usage.input_tokens.toLocaleString()} input
                  tokens
                </span>
              )}
              <span className="decision-meta-model">{run.response.model}</span>
            </div>
            <ul className="decision-answers">
              {Object.entries(run.response.answers).map(([id, answer]) => (
                <AnswerCard key={id} id={id} answer={answer} />
              ))}
            </ul>
            <details className="decision-request">
              <summary>Request</summary>
              <pre>{decisionCurl(run.request, location.origin)}</pre>
              <Copy
                label="Copy as curl"
                text={() => decisionCurl(run.request, location.origin)}
              />
            </details>
          </>
        ) : (
          !running &&
          !error && (
            <div className="studio-placeholder decision-placeholder">
              <h1>Decision lab</h1>
              <p>
                Describe a state, ask choice, score or yes/no questions, and get
                calibrated probabilities from a decision model in one pass.
              </p>
            </div>
          )
        )}
        {running && (
          <div className="studio-busy" role="status">
            <span className="studio-spinner" aria-hidden="true" />
            Deciding…
          </div>
        )}
      </section>
    </div>
  );
}

function QuestionEditor({
  draft,
  index,
  onChange,
  onRemove,
}: {
  draft: DecisionQuestionDraft;
  index: number;
  onChange: (
    change: (draft: DecisionQuestionDraft) => DecisionQuestionDraft,
  ) => void;
  onRemove?: () => void;
}) {
  const label = draft.id.trim() || `Question ${index + 1}`;
  return (
    <li className="decision-question" aria-label={label}>
      <div className="decision-question-head">
        <label className="decision-field decision-id">
          <span>Id</span>
          <input
            value={draft.id}
            placeholder="urgent"
            spellCheck={false}
            autoCapitalize="off"
            onChange={(event) => {
              const id = event.target.value;
              onChange((item) => ({ ...item, id }));
            }}
          />
        </label>
        <div
          className="studio-segmented decision-type"
          role="group"
          aria-label={`${label} type`}
        >
          {decisionQuestionTypes.map((type) => (
            <button
              key={type}
              type="button"
              aria-pressed={draft.type === type}
              onClick={() => onChange((item) => retypeQuestion(item, type))}
            >
              {decisionQuestionTypeLabels[type]}
            </button>
          ))}
        </div>
        {onRemove && (
          <button
            type="button"
            className="decision-remove"
            aria-label={`Remove ${label}`}
            title="Remove question"
            onClick={onRemove}
          >
            ×
          </button>
        )}
      </div>
      <label className="decision-field">
        <span>Question</span>
        <input
          value={draft.instructions}
          placeholder={
            draft.type === "noul"
              ? "Is a service down?"
              : draft.type === "score"
                ? "How urgent is this?"
                : "Which team should handle the message?"
          }
          onChange={(event) => {
            const instructions = event.target.value;
            onChange((item) => ({ ...item, instructions }));
          }}
        />
      </label>
      <p className="generation-settings-hint">{typeHints[draft.type]}</p>
      {draft.type === "choice" && (
        <div
          className="decision-rows"
          role="group"
          aria-label={`${label} options`}
        >
          {draft.options.map((option, n) => (
            <div className="decision-row decision-option" key={n}>
              <input
                aria-label={`Option ${n + 1} id`}
                value={option.key}
                placeholder="id"
                spellCheck={false}
                autoCapitalize="off"
                onChange={(event) => {
                  const key = event.target.value;
                  onChange((item) => ({
                    ...item,
                    options: item.options.map((value, m) =>
                      m === n ? { ...value, key } : value,
                    ),
                  }));
                }}
              />
              <input
                aria-label={`Option ${n + 1} description`}
                value={option.description}
                placeholder="Description · optional"
                onChange={(event) => {
                  const description = event.target.value;
                  onChange((item) => ({
                    ...item,
                    options: item.options.map((value, m) =>
                      m === n ? { ...value, description } : value,
                    ),
                  }));
                }}
              />
              <button
                type="button"
                aria-label={`Remove option ${n + 1}`}
                disabled={draft.options.length <= 2}
                onClick={() =>
                  onChange((item) => ({
                    ...item,
                    options: item.options.filter((_, m) => m !== n),
                  }))
                }
              >
                ×
              </button>
            </div>
          ))}
          <button
            type="button"
            className="decision-add-row"
            disabled={draft.options.length >= maxDecisionOptions}
            onClick={() =>
              onChange((item) => ({
                ...item,
                options: [...item.options, { key: "", description: "" }],
              }))
            }
          >
            + Option
          </button>
        </div>
      )}
      {draft.type === "score" && (
        <div
          className="decision-rows"
          role="group"
          aria-label={`${label} levels`}
        >
          {draft.levels.map((level, n) => (
            <div className="decision-row decision-level" key={n}>
              <span className="decision-level-index" aria-hidden="true">
                {n}
              </span>
              <input
                aria-label={`Level ${n}`}
                value={level}
                placeholder={n === 0 ? "Lowest" : `Level ${n}`}
                onChange={(event) => {
                  const value = event.target.value;
                  onChange((item) => ({
                    ...item,
                    levels: item.levels.map((entry, m) =>
                      m === n ? value : entry,
                    ),
                  }));
                }}
              />
              <button
                type="button"
                aria-label={`Remove level ${n}`}
                disabled={draft.levels.length <= 2}
                onClick={() =>
                  onChange((item) => ({
                    ...item,
                    levels: item.levels.filter((_, m) => m !== n),
                  }))
                }
              >
                ×
              </button>
            </div>
          ))}
          <button
            type="button"
            className="decision-add-row"
            disabled={draft.levels.length >= 10}
            onClick={() =>
              onChange((item) => ({ ...item, levels: [...item.levels, ""] }))
            }
          >
            + Level
          </button>
        </div>
      )}
      {draft.type === "noul" && (
        <div className="decision-rows decision-noul">
          <label className="decision-field">
            <span>Yes means · optional</span>
            <input
              value={draft.whenTrue}
              onChange={(event) => {
                const whenTrue = event.target.value;
                onChange((item) => ({ ...item, whenTrue }));
              }}
            />
          </label>
          <label className="decision-field">
            <span>No means · optional</span>
            <input
              value={draft.whenFalse}
              onChange={(event) => {
                const whenFalse = event.target.value;
                onChange((item) => ({ ...item, whenFalse }));
              }}
            />
          </label>
        </div>
      )}
    </li>
  );
}

function AnswerCard({ id, answer }: { id: string; answer: DecisionAnswer }) {
  const bars = decisionBars(answer);
  const closest =
    answer.type === "score"
      ? answer.legend[String(Math.round(answer.score))]
      : undefined;
  return (
    <li className="decision-answer">
      <div className="decision-answer-head">
        <span className="decision-answer-id">{id}</span>
        <span className="decision-answer-type">
          {decisionQuestionTypeLabels[answer.type]}
        </span>
      </div>
      <div className="decision-answer-value">
        <strong>{decisionHeadline(answer)}</strong>
        {closest && <span>closest: {closest}</span>}
        <span>
          {answer.type === "noul" ? "P(yes) " : "confidence "}
          {formatPercent(
            answer.type === "noul" ? answer.noul : decisionConfidence(answer),
          )}
        </span>
      </div>
      <ul className="decision-bars" aria-label={`${id} probabilities`}>
        {bars.map((bar) => (
          <li key={bar.key} className={bar.selected ? "selected" : undefined}>
            <span className="decision-bar-label" title={bar.label}>
              {bar.label}
            </span>
            <span className="decision-bar-track" aria-hidden="true">
              <span
                className="decision-bar-fill"
                style={{ width: `${(bar.probability * 100).toFixed(1)}%` }}
              />
            </span>
            <span className="decision-bar-value">
              {formatPercent(bar.probability)}
            </span>
          </li>
        ))}
      </ul>
    </li>
  );
}
