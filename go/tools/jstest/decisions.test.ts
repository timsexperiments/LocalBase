// Decisions: the words for an answer, its request, and the batch export.
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import { BANDS, band, batchCSV, buildRequest, csvCell, curlSnippet, jsSnippet, needsReview, percent, problem, readAnswer, situations } from "../../internal/web/static/js/decision-read.js";

describe("band", () => {
  // Provisional, measured on one model (DESIGN §22.17).
  test("edges", () => {
    expect(band(0.85, 0.15)).toBe("strong"); // gap exactly 0.70
    expect(band(0.84, 0.15)).toBe("leaning"); // gap 0.69
    expect(band(0.62, 0.37)).toBe("leaning"); // gap exactly 0.25
    expect(band(0.61, 0.37)).toBe("close"); // gap 0.24
    expect(band(0.5, 0.5)).toBe("close"); // top exactly 0.50
    expect(band(0.49, 0.2)).toBe("unsure");
    expect(band(0.34, 0.33)).toBe("unsure"); // a three-way tie
  });
  test("a model can have its own cuts", () => {
    BANDS.other = { strong: 0.2, close: 0.05, unsure: 0.1 };
    expect(band(0.6, 0.3, "other")).toBe("strong");
    delete BANDS.other;
    expect(band(0.6, 0.3, "other")).toBe("leaning");
  });
});

describe("readAnswer", () => {
  test("pick one goes from most to least likely", () => {
    const r = readAnswer({ type: "choice", choice: "b", probabilities: { a: 0.06, b: 0.91, c: 0.03 } });
    expect(r.rows.map((x) => x.label)).toEqual(["b", "a", "c"]);
    expect([r.word, r.verdict, r.odds]).toEqual(["Strongly favored", "b", "91%"]);
    expect(r.rows[0].top).toBe(true);
  });
  test("a close call names both", () => {
    const r = readAnswer({ type: "choice", probabilities: { a: 0.52, b: 0.44, c: 0.04 } });
    expect([r.word, r.verdict, r.odds]).toEqual(["Close call", "a or b", "52% and 44%"]);
    expect(needsReview(r)).toBe(true);
  });
  test("yes or no", () => {
    const r = readAnswer({ type: "noul", noul: 0.2 });
    expect([r.word, r.verdict, r.odds]).toEqual(["Leaning", "No", "80%"]);
    expect(r.rows.map((x) => x.label)).toEqual(["No", "Yes"]);
    expect(needsReview(r)).toBe(false);
  });
  test("a scale keeps its order and takes its names from the legend", () => {
    for (const legend of [["low", "medium", "high"], { 0: "low", 1: "medium", 2: "high" }]) {
      const r = readAnswer({ type: "score", score: 2, probabilities: { 0: 0.05, 1: 0.1, 2: 0.85 }, legend });
      expect(r.rows.map((x) => x.label)).toEqual(["low", "medium", "high"]);
      expect(r.answer).toBe("high");
      expect(r.rows[2].top).toBe(true);
    }
  });
  test("no answer is not a result", () => {
    expect(readAnswer(undefined)).toBeNull();
    expect(readAnswer({ type: "choice", probabilities: {} })).toBeNull();
  });
  test("percent", () => {
    expect([percent(0.72), percent(0.002), percent(0.998), percent(0), percent(1)]).toEqual(["72%", "under 1%", "over 99%", "0%", "100%"]);
  });
});

const form = (more = {}) => ({ type: "choice", question: "Which?", situation: "", batch: false, choice: [{ name: "a", note: "first" }, { name: "b", note: "" }, { name: " ", note: "" }], score: [{ name: "low" }, { name: "high" }], ...more });

describe("the request", () => {
  test("each type, exactly what the API takes", () => {
    expect(buildRequest(form({ situation: " text " }), " text ", "m")).toEqual({ model: "m", state: "text", questions: { answer: { type: "choice", instructions: "Which?", criteria: { a: "first", b: null } } } });
    expect(buildRequest(form({ type: "score" }), "", "m").questions.answer.criteria).toEqual(["low", "high"]);
    expect(buildRequest(form({ type: "noul" }), "", "m").questions.answer).toEqual({ type: "noul", instructions: "Which?" });
    expect("state" in buildRequest(form(), "  ", "m")).toBe(false);
  });
  test("problems name their field", () => {
    expect(problem(form())).toBeNull();
    expect(problem(form({ question: " " }))?.field).toBe("question");
    expect(problem(form({ choice: [{ name: "a" }] }))?.field).toBe("options");
    expect(problem(form({ choice: [{ name: "a" }, { name: "a" }] }))?.field).toBe("options");
    expect(problem(form({ type: "noul", choice: [] }))).toBeNull();
    expect(problem(form({ batch: true }))?.field).toBe("situation");
    expect(problem(form({ batch: true, situation: "x\n".repeat(201) }))?.message).toContain("201");
  });
  test("code holds the placeholder, and survives a quote", () => {
    const body = buildRequest(form({ question: "Isn't it?" }), "", "m");
    const curl = curlSnippet(body, "http://x");
    expect(curl).toContain("$LOCALBASE_API_KEY");
    expect(curl).toContain(`Isn'\\''t it?`);
    expect(jsSnippet(body, "http://x")).toContain("process.env.LOCALBASE_API_KEY");
  });
});

describe("a batch", () => {
  test("situations: one per line, blanks and a BOM gone, quotes undone", () => {
    expect(situations('﻿one\r\n\r\n  two  \n"say ""hi"", ok"\n')).toEqual(["one", "two", 'say "hi", ok']);
  });
  test("a formula cannot run from the export", () => {
    for (const bad of ["=1+1", "+1", "-1", "@x"]) expect(csvCell(bad)).toBe(`'${bad}`);
    expect(csvCell('a,"b"')).toBe('"a,""b"""');
  });
  test("CSV has a column per answer, and failures keep their row", () => {
    const read = readAnswer({ type: "choice", probabilities: { a: 0.9, b: 0.1 } });
    const csv = batchCSV([{ situation: "one", read }, { situation: "two", error: "No" }]);
    expect(csv.split("\r\n")).toEqual(["situation,answer,read,a,b,error", "one,a,Strongly favored,0.9000,0.1000,", "two,,,,,No", ""]);
  });
});
