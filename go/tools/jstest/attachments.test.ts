// The Go UI's attachments against TS attachments.ts.
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/attachments.js";
import * as ts from "../../../src/ui/attachments";

// The Go page's model JSON and the TS UI's model for the same inputs.
const model = (inputs: string[]) => ({ go: { inputs }, ts: { catalog: { kind: "llm", inputModalities: inputs, outputModalities: ["text"] } } as any });
const textOnly = model(["text"]);
const vision = model(["text", "image"]);
const png = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13]);
const jpeg = new Uint8Array([255, 216, 255, 224, 0, 16]);
const webp = new Uint8Array([...new TextEncoder().encode("RIFF"), 0, 0, 0, 0, ...new TextEncoder().encode("WEBPVP8 ")]);
const file = (name: string, bytes: Uint8Array | string, type = "") => new File([bytes], name, { type });
const noIds = (list: any[]) => list.map(({ id, ...rest }) => rest);

async function outcome(fn: () => Promise<any[]>) {
  try {
    return { ok: noIds(await fn()) };
  } catch (e) {
    return { error: (e as Error).message };
  }
}

describe("readAttachments", () => {
  const cases: [string, File[], any, any[]?, any[]?][] = [
    ["text file", [file("notes.md", "# Hi\nthere")], textOnly],
    ["code files", [file("a.go", "package a"), file("b.py", "print(1)")], textOnly],
    ["png, jpeg and webp", [file("a.png", png, "image/png"), file("b.jpg", jpeg), file("c.webp", webp)], vision],
    ["image without vision", [file("a.png", png, "image/png")], textOnly],
    ["image that is not an image", [file("fake.png", "not an image")], vision],
    ["pdf", [file("doc.pdf", "%PDF-1.4")], textOnly],
    ["empty", [file("empty.txt", "")], textOnly],
    ["too big text", [file("big.txt", "x".repeat(128 * 1024 + 1))], textOnly],
    ["total text over 256 KiB", [file("a.txt", "x".repeat(100_000)), file("b.txt", "x".repeat(100_000)), file("c.txt", "x".repeat(100_000))], textOnly],
    ["not UTF-8", [file("bad.txt", new Uint8Array([0xff, 0xfe, 0x41]))], textOnly],
    ["NUL byte", [file("bin.txt", "a\0b")], textOnly],
    ["five files", Array.from({ length: 5 }, (_, i) => file(`${i}.txt`, "x")), textOnly],
    ["over the count with existing", [file("a.txt", "x")], textOnly, [1, 2, 3, 4].map((i) => ({ id: `${i}`, kind: "text", name: `${i}.txt`, size: 1, text: "x" }))],
    ["retained storage full", [file("a.png", png, "image/png")], vision, [], [{ id: "r", kind: "image", name: "r.png", size: 1, url: "x".repeat(16 * 1024 * 1024) }]],
  ];
  for (const [name, files, m, existing = [], retained = []] of cases)
    test(name, async () => {
      const got = await outcome(() => go.readAttachments(files, m.go, existing, retained));
      expect(got).toEqual(await outcome(() => ts.readAttachments(files, m.ts, existing as any, retained as any)));
    });
});

describe("checks", () => {
  test("accept", () => {
    for (const m of [textOnly, vision, model([])]) expect(go.attachmentAccept(m.go)).toBe(ts.attachmentAccept(m.ts));
  });
  const image = { id: "i", kind: "image", name: "a.png", size: 10, url: "data:image/png;base64,AAAA" };
  const text = { id: "t", kind: "text", name: "a.txt", size: 5, text: "hello" };
  const conversations: [string, any[]][] = [
    ["plain", [{ id: "1", role: "user", text: "hi" }]],
    ["image", [{ id: "1", role: "user", text: "see", attachments: [image] }]],
    ["text", [{ id: "1", role: "user", text: "", attachments: [text] }]],
    ["missing", [{ id: "1", role: "user", text: "x", attachmentsMissing: true }]],
    ["too large", [{ id: "1", role: "user", text: "x".repeat(21 * 1024 * 1024) }]],
  ];
  for (const [name, messages] of conversations)
    test(name, () => {
      for (const m of [textOnly, vision]) {
        expect(go.conversationAttachmentError(messages, m.go)).toBe(ts.conversationAttachmentError(messages, m.ts));
        let goError, tsError;
        try { goError = go.chatRequestError(messages, m.go); } catch (e) { goError = (e as Error).message; }
        try { tsError = ts.chatRequestError(messages, m.ts); } catch (e) { tsError = (e as Error).message; }
        expect(goError).toBe(tsError);
      }
    });
});
