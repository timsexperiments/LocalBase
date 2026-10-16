import { describe, expect, test } from "bun:test";
import {
  blankQuestion,
  buildDecisionRequest,
  decisionBars,
  decisionConfidence,
  decisionCurl,
  decisionExamples,
  decisionHeadline,
  decisionResponseSchema,
  decisionState,
  draftsFromJson,
  exampleDrafts,
  formatLatency,
  formatPercent,
  looseQuestionsJson,
  parseQuestionsJson,
  questionsJson,
  retypeQuestion,
  type DecisionAnswer,
  type DecisionQuestionDraft,
} from "./decision";

function draft(
  type: DecisionQuestionDraft["type"],
  id: string,
  change: Partial<DecisionQuestionDraft> = {},
): DecisionQuestionDraft {
  return { ...blankQuestion(type, id), ...change };
}

describe("questionsJson", () => {
  test("builds every question type", () => {
    const built = questionsJson([
      draft("choice", " department ", {
        instructions: " Which team? ",
        options: [
          { key: "billing", description: "Payments" },
          { key: "technical", description: " " },
          { key: "", description: "" },
        ],
      }),
      draft("score", "urgency", { levels: ["Can wait", "Today"] }),
      draft("noul", "outage", { whenTrue: "A service is down" }),
      draft("noul", "plain"),
    ]);
    expect(built).toEqual({
      ok: true,
      value: {
        department: {
          type: "choice",
          instructions: "Which team?",
          criteria: { billing: "Payments", technical: null },
        },
        urgency: { type: "score", criteria: ["Can wait", "Today"] },
        outage: { type: "noul", criteria: { true: "A service is down" } },
        plain: { type: "noul" },
      },
    });
  });

  test("names the first problem", () => {
    const error = (drafts: DecisionQuestionDraft[]) => {
      const built = questionsJson(drafts);
      return built.ok ? "" : built.error;
    };
    expect(error([])).toBe("Add at least one question.");
    expect(error([draft("noul", " ")])).toBe("Question 1 needs an id.");
    expect(error([draft("noul", "a"), draft("noul", "a")])).toBe(
      "Two questions use the id “a”.",
    );
    expect(error([draft("choice", "a")])).toBe("“a” needs at least 2 options.");
    expect(
      error([
        draft("choice", "a", {
          options: [
            { key: "x", description: "" },
            { key: "", description: "Unnamed" },
          ],
        }),
      ]),
    ).toBe("Every option of “a” needs an id.");
    expect(
      error([
        draft("choice", "a", {
          options: [
            { key: "x", description: "" },
            { key: " x", description: "" },
          ],
        }),
      ]),
    ).toBe("“a” has two options named “x”.");
    expect(error([draft("score", "s", { levels: ["only"] })])).toBe(
      "“s” needs 2 to 10 levels.",
    );
    expect(
      error([draft("score", "s", { levels: Array(11).fill("level") })]),
    ).toBe("“s” needs 2 to 10 levels.");
    expect(error([draft("score", "s")])).toBe(
      "Describe every level of “s”, or remove the empty ones.",
    );
  });
});

describe("raw questions JSON", () => {
  test("parses valid questions and rejects malformed ones", () => {
    expect(parseQuestionsJson('{"a":{"type":"noul"}}')).toEqual({
      ok: true,
      value: { a: { type: "noul" } },
    });
    expect(parseQuestionsJson("{").ok).toBe(false);
    expect(parseQuestionsJson("{}")).toEqual({
      ok: false,
      error: "Add at least one question.",
    });
    expect(parseQuestionsJson('{"a":{"type":"maybe"}}').ok).toBe(false);
    expect(parseQuestionsJson('{"a":{"type":"noul","extra":1}}').ok).toBe(
      false,
    );
    expect(
      parseQuestionsJson('{"a":{"type":"noul","instructions":{"focus":"x"}}}')
        .ok,
    ).toBe(true);
  });

  test("round-trips through the builder", () => {
    for (const example of decisionExamples) {
      const drafts = draftsFromJson(JSON.stringify(example.questions));
      expect(drafts).not.toBeNull();
      expect(questionsJson(drafts!)).toEqual({
        ok: true,
        value: example.questions,
      });
      expect(questionsJson(exampleDrafts(example))).toEqual({
        ok: true,
        value: example.questions,
      });
      expect(JSON.parse(looseQuestionsJson(drafts!))).toEqual(
        example.questions,
      );
    }
  });

  test("leaves structured instructions to the JSON editor", () => {
    expect(
      draftsFromJson('{"a":{"type":"noul","instructions":["one","two"]}}'),
    ).toBeNull();
    expect(draftsFromJson("not json")).toBeNull();
  });

  test("keeps unfinished drafts when switching to JSON", () => {
    expect(
      JSON.parse(
        looseQuestionsJson([
          draft("choice", ""),
          draft("score", "", { levels: ["Low", " "] }),
          draft("noul", "x", { whenFalse: "Nothing is down" }),
        ]),
      ),
    ).toEqual({
      question_1: {
        type: "choice",
        criteria: { option_1: null, option_2: null },
      },
      question_2: { type: "score", criteria: ["Low", ""] },
      x: { type: "noul", criteria: { false: "Nothing is down" } },
    });
  });

  test("retyping keeps the id, instructions and existing rows", () => {
    const choice = draft("choice", "a", {
      instructions: "Pick",
      options: [
        { key: "x", description: "" },
        { key: "y", description: "" },
      ],
    });
    const score = retypeQuestion(choice, "score");
    expect(score).toMatchObject({
      uid: choice.uid,
      id: "a",
      type: "score",
      instructions: "Pick",
      levels: ["", "", ""],
    });
    expect(retypeQuestion(score, "choice").options).toEqual(choice.options);
    expect(retypeQuestion(choice, "choice")).toBe(choice);
  });
});

describe("decision requests", () => {
  test("reads the state as text or JSON", () => {
    expect(decisionState("Orders are blocked", "text")).toEqual({
      ok: true,
      value: "Orders are blocked",
    });
    expect(decisionState(" ", "text").ok).toBe(false);
    expect(decisionState('{"total":3}', "json")).toEqual({
      ok: true,
      value: { total: 3 },
    });
    expect(decisionState("[1, null]", "json")).toEqual({
      ok: true,
      value: [1, null],
    });
    expect(decisionState("{", "json").ok).toBe(false);
    expect(decisionState("", "json").ok).toBe(false);
  });

  test("builds the body from either editor", () => {
    const inputs = {
      model: "clef-flash-q4_k_m",
      format: "text" as const,
      state: "Checkout is down",
      images: [],
      editor: "builder" as const,
      drafts: [draft("noul", "outage")],
      questionsText: '{"a":{"type":"noul"}}',
    };
    expect(buildDecisionRequest(inputs)).toEqual({
      ok: true,
      value: {
        model: "clef-flash-q4_k_m",
        state: "Checkout is down",
        questions: { outage: { type: "noul" } },
      },
    });
    expect(
      buildDecisionRequest({
        ...inputs,
        editor: "json",
        images: ["data:image/png;base64,AAAA"],
      }),
    ).toEqual({
      ok: true,
      value: {
        model: "clef-flash-q4_k_m",
        state: "Checkout is down",
        images: ["data:image/png;base64,AAAA"],
        questions: { a: { type: "noul" } },
      },
    });
    expect(buildDecisionRequest({ ...inputs, state: "" }).ok).toBe(false);
    expect(buildDecisionRequest({ ...inputs, drafts: [] }).ok).toBe(false);
    expect(
      buildDecisionRequest({ ...inputs, images: Array(5).fill("data:,") }),
    ).toEqual({ ok: false, error: "Attach at most 4 images." });
  });

  test("copies as a shell-safe curl command with shortened images", () => {
    const curl = decisionCurl(
      {
        model: "clef",
        state: "It's down",
        images: ["data:image/png;base64,AAAABBBB"],
        questions: { a: { type: "noul" } },
      },
      "https://localbase.example",
    );
    expect(curl).toBe(
      [
        "curl 'https://localbase.example/v1/systemone' \\",
        '  -H "Authorization: Bearer $LOCALBASE_API_KEY" \\',
        "  -H 'Content-Type: application/json' \\",
        `  -d '{"model":"clef","state":"It'\\''s down","images":["data:image/png;base64,<base64>"],"questions":{"a":{"type":"noul"}}}'`,
      ].join("\n"),
    );
    expect(curl).not.toContain("AAAABBBB");
  });
});

describe("decision results", () => {
  const response = decisionResponseSchema.parse({
    model: "clef-flash-q4_k_m",
    answers: {
      department: {
        type: "choice",
        choice: "technical",
        probabilities: { billing: 0.039, technical: 0.961 },
        confidence: 0.961,
      },
      urgency: {
        type: "score",
        score: 1.78,
        legend: { "0": "Can wait", "1": "This week", "2": "Today" },
        probabilities: { "2": 0.8, "0": 0.02, "1": 0.18 },
        confidence: 0.8,
      },
      outage: { type: "noul", noul: 0.948 },
    },
  });
  const answer = (id: string) => response.answers[id] as DecisionAnswer;

  test("parses responses without usage", () => {
    expect(response.usage).toBeUndefined();
  });

  test("draws a bar per option and highlights the answer", () => {
    expect(decisionBars(answer("department"))).toEqual([
      { key: "billing", label: "billing", probability: 0.039, selected: false },
      {
        key: "technical",
        label: "technical",
        probability: 0.961,
        selected: true,
      },
    ]);
    expect(
      decisionBars(answer("urgency")).map((bar) => [bar.label, bar.selected]),
    ).toEqual([
      ["0 · Can wait", false],
      ["1 · This week", false],
      ["2 · Today", true],
    ]);
    const noul = decisionBars(answer("outage"));
    expect(noul.map((bar) => [bar.label, bar.selected])).toEqual([
      ["Yes", true],
      ["No", false],
    ]);
    expect(noul[1]!.probability).toBeCloseTo(0.052);
  });

  test("summarizes each answer", () => {
    expect(decisionHeadline(answer("department"))).toBe("technical");
    expect(decisionHeadline(answer("urgency"))).toBe("1.78 of 0–2");
    expect(decisionHeadline(answer("outage"))).toBe("Yes");
    expect(decisionHeadline({ type: "noul", noul: 0.2 })).toBe("No");
    expect(decisionConfidence(answer("department"))).toBe(0.961);
    expect(decisionConfidence({ type: "noul", noul: 0.2 })).toBe(0.8);
    expect(formatPercent(0.9714)).toBe("97.1%");
    expect(formatPercent(Number.NaN)).toBe("0.0%");
    expect(formatLatency(950.4)).toBe("950 ms");
    expect(formatLatency(1234)).toBe("1.23 s");
  });
});
