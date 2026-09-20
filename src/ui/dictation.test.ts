import { expect, spyOn, test } from "bun:test";
import { appendDictation, transcribeDictation } from "./dictation";
import { encodePcm16Wav } from "./dictation-audio";
import { videoParameters } from "./generation-settings";

test("dictation appends without discarding typed text or existing whitespace", () => {
  expect(appendDictation("", "  Hello  ")).toBe("Hello");
  expect(appendDictation("Typed", "speech")).toBe("Typed speech");
  expect(appendDictation("Typed\n", "speech")).toBe("Typed\nspeech");
  expect(appendDictation("Typed", "   ")).toBe("Typed");
  const longPrompt = appendDictation("x".repeat(16384), "extra speech");
  expect(longPrompt).toEndWith(" extra speech");
  expect(() => videoParameters({ negative_prompt: longPrompt })).toThrow();
});

test("dictation sends WAV to the selected local STT model through the browser session", async () => {
  const request = spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ text: "Spoken text" }),
  );
  const abort = new AbortController();
  try {
    const audio = encodePcm16Wav(new Float32Array([0, 0.5, -0.5]));
    expect(
      await transcribeDictation(
        audio,
        "whisper-base-q8_0",
        { kind: "session" },
        abort.signal,
      ),
    ).toBe("Spoken text");
    const [path, init] = request.mock.calls[0] ?? [];
    expect(path).toBe("/app/api/v1/audio/transcriptions");
    expect(init?.method).toBe("POST");
    expect(init?.signal).toBe(abort.signal);
    const form = init?.body;
    if (!(form instanceof FormData))
      throw new Error("Expected multipart audio");
    expect(form.get("model")).toBe("whisper-base-q8_0");
    expect(form.get("response_format")).toBe("json");
    const file = form.get("file");
    if (!(file instanceof File)) throw new Error("Expected recorded WAV");
    expect(file.name).toBe("dictation.wav");
    expect(file.type).toBe("audio/wav");
    expect(new Uint8Array(await file.arrayBuffer())).toEqual(
      new Uint8Array(await audio.arrayBuffer()),
    );
    expect([...form.keys()]).toEqual(["model", "response_format", "file"]);
  } finally {
    request.mockRestore();
  }
});
