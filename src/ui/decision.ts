import { z } from "zod";

/** The question types `/v1/systemone` accepts. */
export const decisionQuestionTypes = ["choice", "score", "noul"] as const;
export type DecisionQuestionType = (typeof decisionQuestionTypes)[number];
export const decisionQuestionTypeLabels: Record<DecisionQuestionType, string> =
  {
    choice: "Choice",
    score: "Score",
    noul: "Yes / no",
  };

/** One option of a choice question: an id and an optional description. */
export type DecisionOption = { key: string; description: string };
/** A question as the builder edits it, before it becomes request JSON. */
export type DecisionQuestionDraft = {
  /** Stable React key; not sent. */
  uid: string;
  id: string;
  type: DecisionQuestionType;
  instructions: string;
  options: DecisionOption[];
  levels: string[];
  whenTrue: string;
  whenFalse: string;
};

export type DecisionStateFormat = "text" | "json";

/** The most choice options any llama.cpp decision architecture accepts. */
export const maxDecisionOptions = 255;
const maxScoreLevels = 10;
const maxDecisionQuestions = 64;
/** Same-size limit as the gateway applies to one inline image. */
export const maxDecisionImageBytes = 10 * 1024 * 1024;
export const maxDecisionImages = 4;

let nextUid = 0;
function uid() {
  nextUid += 1;
  return `q${nextUid}`;
}

/** A blank question of `type`, with the minimum options it needs. */
export function blankQuestion(
  type: DecisionQuestionType,
  id = "",
): DecisionQuestionDraft {
  return {
    uid: uid(),
    id,
    type,
    instructions: "",
    options:
      type === "choice"
        ? [
            { key: "", description: "" },
            { key: "", description: "" },
          ]
        : [],
    levels: type === "score" ? ["", "", ""] : [],
    whenTrue: "",
    whenFalse: "",
  };
}

/** Changes a question's type, keeping its id and instructions. */
export function retypeQuestion(
  question: DecisionQuestionDraft,
  type: DecisionQuestionType,
): DecisionQuestionDraft {
  if (question.type === type) return question;
  const blank = blankQuestion(type, question.id);
  return {
    ...blank,
    uid: question.uid,
    instructions: question.instructions,
    options: question.options.length ? question.options : blank.options,
    levels: question.levels.length ? question.levels : blank.levels,
    whenTrue: question.whenTrue,
    whenFalse: question.whenFalse,
  };
}

export type DecisionQuestionJson =
  | {
      type: "choice";
      instructions?: string;
      criteria: Record<string, string | null>;
    }
  | { type: "score"; instructions?: string; criteria: string[] }
  | {
      type: "noul";
      instructions?: string;
      criteria?: { true?: string; false?: string };
    };
export type DecisionQuestionsJson = Record<string, DecisionQuestionJson>;

type Built<T> = { ok: true; value: T } | { ok: false; error: string };

/** Turns builder drafts into the `questions` object, or names the first problem. */
export function questionsJson(
  drafts: readonly DecisionQuestionDraft[],
): Built<DecisionQuestionsJson> {
  if (!drafts.length) return { ok: false, error: "Add at least one question." };
  if (drafts.length > maxDecisionQuestions)
    return {
      ok: false,
      error: `Use at most ${maxDecisionQuestions} questions.`,
    };
  const questions: DecisionQuestionsJson = {};
  for (const [index, draft] of drafts.entries()) {
    const id = draft.id.trim();
    const name = id ? `“${id}”` : `Question ${index + 1}`;
    if (!id) return { ok: false, error: `${name} needs an id.` };
    if (Object.hasOwn(questions, id))
      return { ok: false, error: `Two questions use the id “${id}”.` };
    const instructions = draft.instructions.trim();
    const base = instructions ? { instructions } : {};
    if (draft.type === "choice") {
      const options = draft.options.filter(
        (option) => option.key.trim() || option.description.trim(),
      );
      if (options.length < 2)
        return { ok: false, error: `${name} needs at least 2 options.` };
      if (options.length > maxDecisionOptions)
        return {
          ok: false,
          error: `${name} accepts at most ${maxDecisionOptions} options.`,
        };
      const criteria: Record<string, string | null> = {};
      for (const option of options) {
        const key = option.key.trim();
        if (!key)
          return { ok: false, error: `Every option of ${name} needs an id.` };
        if (Object.hasOwn(criteria, key))
          return {
            ok: false,
            error: `${name} has two options named “${key}”.`,
          };
        criteria[key] = option.description.trim() || null;
      }
      questions[id] = { type: "choice", ...base, criteria };
    } else if (draft.type === "score") {
      const levels = draft.levels.map((level) => level.trim());
      if (levels.length < 2 || levels.length > maxScoreLevels)
        return {
          ok: false,
          error: `${name} needs 2 to ${maxScoreLevels} levels.`,
        };
      if (levels.some((level) => !level))
        return {
          ok: false,
          error: `Describe every level of ${name}, or remove the empty ones.`,
        };
      questions[id] = { type: "score", ...base, criteria: levels };
    } else {
      const whenTrue = draft.whenTrue.trim();
      const whenFalse = draft.whenFalse.trim();
      questions[id] = {
        type: "noul",
        ...base,
        ...(whenTrue || whenFalse
          ? {
              criteria: {
                ...(whenTrue ? { true: whenTrue } : {}),
                ...(whenFalse ? { false: whenFalse } : {}),
              },
            }
          : {}),
      };
    }
  }
  return { ok: true, value: questions };
}

const instructionsSchema = z.union([
  z.string(),
  z.record(z.string(), z.unknown()),
  z.array(z.unknown()),
]);
const rawQuestionSchema = z.discriminatedUnion("type", [
  z
    .object({
      type: z.literal("choice"),
      instructions: instructionsSchema.optional(),
      criteria: z.record(z.string().min(1), z.string().nullable()),
    })
    .strict(),
  z
    .object({
      type: z.literal("score"),
      instructions: instructionsSchema.optional(),
      criteria: z.array(z.string()),
    })
    .strict(),
  z
    .object({
      type: z.literal("noul"),
      instructions: instructionsSchema.optional(),
      criteria: z
        .object({ true: z.string().optional(), false: z.string().optional() })
        .strict()
        .optional(),
    })
    .strict(),
]);
const rawQuestionsSchema = z.record(z.string().min(1), rawQuestionSchema);

/** Parses raw `questions` JSON well enough to send it; the gateway checks the rest. */
export function parseQuestionsJson(text: string): Built<unknown> {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { ok: false, error: "Questions must be valid JSON." };
  }
  const parsed = rawQuestionsSchema.safeParse(value);
  if (!parsed.success)
    return {
      ok: false,
      error:
        "Questions must be an object of question ids, each with a type of choice, score or noul.",
    };
  if (!Object.keys(parsed.data).length)
    return { ok: false, error: "Add at least one question." };
  return { ok: true, value };
}

/**
 * Builder drafts for raw questions, or null when the JSON uses something the
 * builder cannot show, such as structured instructions.
 */
export function draftsFromJson(text: string): DecisionQuestionDraft[] | null {
  const parsed = parseQuestionsJson(text);
  if (!parsed.ok) return null;
  const questions = rawQuestionsSchema.parse(parsed.value);
  const drafts: DecisionQuestionDraft[] = [];
  for (const [id, question] of Object.entries(questions)) {
    if (
      question.instructions !== undefined &&
      typeof question.instructions !== "string"
    )
      return null;
    const draft = {
      ...blankQuestion(question.type, id),
      instructions: question.instructions ?? "",
    };
    if (question.type === "choice")
      draft.options = Object.entries(question.criteria).map(
        ([key, description]) => ({ key, description: description ?? "" }),
      );
    else if (question.type === "score") draft.levels = [...question.criteria];
    else {
      draft.whenTrue = question.criteria?.true ?? "";
      draft.whenFalse = question.criteria?.false ?? "";
    }
    drafts.push(draft);
  }
  return drafts;
}

/** The request `state`: the text as typed, or parsed JSON. */
export function decisionState(
  text: string,
  format: DecisionStateFormat,
): Built<unknown> {
  if (format === "text") {
    return text.trim()
      ? { ok: true, value: text }
      : { ok: false, error: "Describe the state to decide on." };
  }
  if (!text.trim())
    return { ok: false, error: "Enter the state as JSON, or switch to text." };
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch {
    return {
      ok: false,
      error: "The state is not valid JSON. Fix it or switch to text.",
    };
  }
}

export type DecisionRequest = {
  model: string;
  state: unknown;
  images?: string[];
  questions: unknown;
};

/** Sample decisions from the Clef model card, to start from. */
export const decisionExamples: {
  id: string;
  label: string;
  format: DecisionStateFormat;
  state: string;
  questions: DecisionQuestionsJson;
}[] = [
  {
    id: "support",
    label: "Support triage",
    format: "text",
    state: "Our checkout started returning errors and orders are blocked.",
    questions: {
      department: {
        type: "choice",
        instructions: "Which team should handle the message?",
        criteria: {
          billing: "Payments or invoices",
          technical: "Bugs or outages",
        },
      },
      urgency: {
        type: "score",
        instructions: "urgency",
        criteria: ["Can wait", "This week", "Today"],
      },
      outage: { type: "noul", instructions: "Is a service down?" },
    },
  },
  {
    id: "invoice",
    label: "Invoice status",
    format: "json",
    state: JSON.stringify(
      {
        invoice: {
          vendor: "Acme",
          total: 1250,
          currency: "USD",
          status: "overdue",
        },
      },
      null,
      2,
    ),
    questions: {
      status: {
        type: "choice",
        instructions: "What is the invoice status?",
        criteria: {
          paid: "Invoice is paid.",
          overdue: "Invoice is past due.",
          draft: "Not sent.",
        },
      },
      large: { type: "noul", instructions: "Is the total above 1000 USD?" },
    },
  },
  {
    id: "urgency",
    label: "Urgency check",
    format: "text",
    state: "My Stripe integration keeps failing. Please help ASAP.",
    questions: {
      urgency: {
        type: "noul",
        instructions: "Does this message express urgency?",
      },
    },
  },
];

const probabilitiesSchema = z.record(z.string(), z.number());
export const decisionResponseSchema = z.object({
  model: z.string(),
  answers: z.record(
    z.string(),
    z.discriminatedUnion("type", [
      z.object({
        type: z.literal("choice"),
        choice: z.string(),
        probabilities: probabilitiesSchema,
        confidence: z.number(),
      }),
      z.object({
        type: z.literal("score"),
        score: z.number(),
        legend: z.record(z.string(), z.string()),
        probabilities: probabilitiesSchema,
        confidence: z.number(),
      }),
      z.object({ type: z.literal("noul"), noul: z.number() }),
    ]),
  ),
  usage: z
    .object({ input_tokens: z.number(), output_tokens: z.number() })
    .optional(),
});
export type DecisionResponse = z.infer<typeof decisionResponseSchema>;
export type DecisionAnswer = DecisionResponse["answers"][string];

export type DecisionBar = {
  key: string;
  label: string;
  probability: number;
  selected: boolean;
};

/** One bar per option, in the order the question listed them. */
export function decisionBars(answer: DecisionAnswer): DecisionBar[] {
  if (answer.type === "noul") {
    const yes = clamp(answer.noul);
    return [
      { key: "true", label: "Yes", probability: yes, selected: yes >= 0.5 },
      {
        key: "false",
        label: "No",
        probability: 1 - yes,
        selected: yes < 0.5,
      },
    ];
  }
  if (answer.type === "choice") {
    return Object.entries(answer.probabilities).map(([key, probability]) => ({
      key,
      label: key,
      probability: clamp(probability),
      selected: key === answer.choice,
    }));
  }
  const best = Object.entries(answer.probabilities).reduce<string | null>(
    (top, [key, probability]) =>
      top === null || probability > (answer.probabilities[top] ?? 0)
        ? key
        : top,
    null,
  );
  return Object.entries(answer.probabilities)
    .sort(([a], [b]) => Number(a) - Number(b))
    .map(([key, probability]) => ({
      key,
      label: answer.legend[key] ? `${key} · ${answer.legend[key]}` : key,
      probability: clamp(probability),
      selected: key === best,
    }));
}

/** The headline for an answer, such as `technical` or `1.78 of 0–2`. */
export function decisionHeadline(answer: DecisionAnswer): string {
  if (answer.type === "choice") return answer.choice;
  if (answer.type === "noul") return answer.noul >= 0.5 ? "Yes" : "No";
  const top = Math.max(
    0,
    ...Object.keys(answer.legend)
      .map(Number)
      .filter((value) => Number.isInteger(value)),
  );
  return `${answer.score.toFixed(2)} of 0–${top}`;
}

/** Confidence for choice and score answers; a yes/no answer's own probability. */
export function decisionConfidence(answer: DecisionAnswer): number {
  return answer.type === "noul"
    ? clamp(Math.max(answer.noul, 1 - answer.noul))
    : clamp(answer.confidence);
}

export function formatPercent(value: number): string {
  return `${(clamp(value) * 100).toFixed(1)}%`;
}

export function formatLatency(ms: number): string {
  return ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(2)} s`;
}

function clamp(value: number) {
  return Number.isFinite(value) ? Math.min(1, Math.max(0, value)) : 0;
}

/** Quotes `value` for a POSIX shell. */
function shellQuote(value: string) {
  return `'${value.replaceAll("'", `'\\''`)}'`;
}

/**
 * A `curl` command for the same request against the public API. Image data is
 * shortened so the command stays readable; the key comes from the environment.
 */
export function decisionCurl(request: DecisionRequest, origin: string): string {
  const body = {
    ...request,
    ...(request.images?.length
      ? {
          images: request.images.map(
            (image) => `${image.slice(0, image.indexOf(",") + 1)}<base64>`,
          ),
        }
      : {}),
  };
  return [
    `curl ${shellQuote(`${origin}/v1/systemone`)} \\`,
    `  -H "Authorization: Bearer $LOCALBASE_API_KEY" \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -d ${shellQuote(JSON.stringify(body))}`,
  ].join("\n");
}

/**
 * The drafts as `questions` JSON without validating them, so switching to the
 * JSON editor keeps unfinished work. Missing ids become `question_N`.
 */
export function looseQuestionsJson(
  drafts: readonly DecisionQuestionDraft[],
): string {
  const questions: Record<string, unknown> = {};
  for (const [index, draft] of drafts.entries()) {
    let id = draft.id.trim() || `question_${index + 1}`;
    while (Object.hasOwn(questions, id)) id = `${id}_${index + 1}`;
    const instructions = draft.instructions.trim();
    const base = {
      type: draft.type,
      ...(instructions ? { instructions } : {}),
    };
    if (draft.type === "choice")
      questions[id] = {
        ...base,
        criteria: Object.fromEntries(
          draft.options.map((option, n) => [
            option.key.trim() || `option_${n + 1}`,
            option.description.trim() || null,
          ]),
        ),
      };
    else if (draft.type === "score")
      questions[id] = {
        ...base,
        criteria: draft.levels.map((level) => level.trim()),
      };
    else {
      const whenTrue = draft.whenTrue.trim();
      const whenFalse = draft.whenFalse.trim();
      questions[id] =
        whenTrue || whenFalse
          ? {
              ...base,
              criteria: {
                ...(whenTrue ? { true: whenTrue } : {}),
                ...(whenFalse ? { false: whenFalse } : {}),
              },
            }
          : base;
    }
  }
  return JSON.stringify(questions, null, 2);
}

export type DecisionInputs = {
  model: string;
  format: DecisionStateFormat;
  state: string;
  images: readonly string[];
  editor: "builder" | "json";
  drafts: readonly DecisionQuestionDraft[];
  questionsText: string;
};

/** The `/v1/systemone` request body for the lab's inputs, or the first problem. */
export function buildDecisionRequest(
  inputs: DecisionInputs,
): Built<DecisionRequest> {
  const state = decisionState(inputs.state, inputs.format);
  if (!state.ok) return state;
  const questions =
    inputs.editor === "builder"
      ? questionsJson(inputs.drafts)
      : parseQuestionsJson(inputs.questionsText);
  if (!questions.ok) return questions;
  if (inputs.images.length > maxDecisionImages)
    return { ok: false, error: `Attach at most ${maxDecisionImages} images.` };
  return {
    ok: true,
    value: {
      model: inputs.model,
      state: state.value,
      ...(inputs.images.length ? { images: [...inputs.images] } : {}),
      questions: questions.value,
    },
  };
}

/** Builder drafts for one of the bundled examples. */
export function exampleDrafts(
  example: (typeof decisionExamples)[number],
): DecisionQuestionDraft[] {
  return draftsFromJson(JSON.stringify(example.questions)) ?? [];
}
