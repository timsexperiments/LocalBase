import { describe, expect, test } from "bun:test";
import { streamText } from "./client";
import { deltaReasoning, stripThinking, thinkSplitter } from "./reasoning";

function sse(events: unknown[]) {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        for (const event of events)
          controller.enqueue(
            encoder.encode(
              `data: ${typeof event === "string" ? event : JSON.stringify(event)}\n\n`,
            ),
          );
        controller.close();
      },
    }),
  );
}
const delta = (value: Record<string, unknown>) => ({
  choices: [{ index: 0, delta: value }],
});

function split(chunks: string[]) {
  const splitter = thinkSplitter();
  let content = "";
  let reasoning = "";
  for (const part of [
    ...chunks.map((c) => splitter.push(c)),
    splitter.flush(),
  ]) {
    content += part.content;
    reasoning += part.reasoning;
  }
  return { content, reasoning };
}

describe("reasoning", () => {
  test("deltaReasoning reads reasoning and reasoning_content", () => {
    expect(deltaReasoning({ reasoning: "a" })).toBe("a");
    expect(deltaReasoning({ reasoning_content: "b" })).toBe("b");
    expect(deltaReasoning({ reasoning: null, reasoning_content: "c" })).toBe(
      "c",
    );
    expect(deltaReasoning({})).toBe("");
  });

  test("thinkSplitter separates a leading think block", () => {
    expect(split(["<think>plan</think>\n\nAnswer"])).toEqual({
      content: "Answer",
      reasoning: "plan",
    });
  });

  test("thinkSplitter handles tags split across chunks", () => {
    expect(
      split(["  <th", "ink>step ", "one</th", "ink>", "Hello", " world"]),
    ).toEqual({ content: "Hello world", reasoning: "step one" });
    expect(split(["<", "think", ">x<", "/", "think>y"])).toEqual({
      content: "y",
      reasoning: "x",
    });
  });

  test("thinkSplitter leaves answers that mention the tag alone", () => {
    expect(split(["Use ", "<think>", " tags"])).toEqual({
      content: "Use <think> tags",
      reasoning: "",
    });
    expect(split(["<thi"])).toEqual({ content: "<thi", reasoning: "" });
  });

  test("thinkSplitter keeps an unclosed think block as reasoning", () => {
    expect(split(["<think>still going</thi"])).toEqual({
      content: "",
      reasoning: "still going</thi",
    });
  });

  test("stripThinking removes closed and trailing think blocks", () => {
    expect(stripThinking("<think>a</think>\n- fact")).toBe("- fact");
    expect(stripThinking("- fact\n<think>unfinished")).toBe("- fact");
  });

  test("streamText reports reasoning_content and reasoning separately", async () => {
    let content = "";
    let reasoning = "";
    await streamText(
      sse([
        delta({ role: "assistant" }),
        delta({ reasoning_content: "Let me " }),
        delta({ reasoning: "think." }),
        delta({ content: "Hi" }),
        delta({ content: " there" }),
        "[DONE]",
      ]),
      (text) => {
        content += text;
      },
      (text) => {
        reasoning += text;
      },
    );
    expect(reasoning).toBe("Let me think.");
    expect(content).toBe("Hi there");
  });

  test("streamText splits inline think tags out of content", async () => {
    let content = "";
    let reasoning = "";
    await streamText(
      sse([
        delta({ content: "<thi" }),
        delta({ content: "nk>hmm</think>" }),
        delta({ content: "Done" }),
        "[DONE]",
      ]),
      (text) => {
        content += text;
      },
      (text) => {
        reasoning += text;
      },
    );
    expect(reasoning).toBe("hmm");
    expect(content).toBe("Done");
  });
});
