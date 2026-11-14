// The inputs of attachments.test.ts, shared by the test (the Go side) and the fixture generator
// (the frozen TS side). See frozen.ts.
import { outcome } from "./frozen.ts";

export type Side = "go" | "ts";
// impl is attachments.js, or TS attachments.ts, with the same four functions.
export type Impl = {
  readAttachments(files: File[], model: any, existing: any[], retained: any[]): Promise<any[]>;
  attachmentAccept(model: any): string;
  conversationAttachmentError(messages: any[], model: any): unknown;
  chatRequestError(messages: any[], model: any): unknown;
};

// The Go page's model JSON and the TS UI's model for the same inputs.
const model = (inputs: string[]) => ({ go: { inputs }, ts: { catalog: { kind: "llm", inputModalities: inputs, outputModalities: ["text"] } } as any });
const textOnly = model(["text"]);
const vision = model(["text", "image"]);
const png = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13]);
const jpeg = new Uint8Array([255, 216, 255, 224, 0, 16]);
const webp = new Uint8Array([...new TextEncoder().encode("RIFF"), 0, 0, 0, 0, ...new TextEncoder().encode("WEBPVP8 ")]);
const file = (name: string, bytes: Uint8Array | string, type = "") => new File([bytes], name, { type });
const noIds = (list: any[]) => list.map(({ id, ...rest }) => rest);

export const readCases: [string, () => File[], ReturnType<typeof model>, any[]?, any[]?][] = [
  ["text file", () => [file("notes.md", "# Hi\nthere")], textOnly],
  ["code files", () => [file("a.go", "package a"), file("b.py", "print(1)")], textOnly],
  ["png, jpeg and webp", () => [file("a.png", png, "image/png"), file("b.jpg", jpeg), file("c.webp", webp)], vision],
  ["image without vision", () => [file("a.png", png, "image/png")], textOnly],
  ["image that is not an image", () => [file("fake.png", "not an image")], vision],
  ["pdf", () => [file("doc.pdf", "%PDF-1.4")], textOnly],
  ["empty", () => [file("empty.txt", "")], textOnly],
  ["too big text", () => [file("big.txt", "x".repeat(128 * 1024 + 1))], textOnly],
  ["total text over 256 KiB", () => [file("a.txt", "x".repeat(100_000)), file("b.txt", "x".repeat(100_000)), file("c.txt", "x".repeat(100_000))], textOnly],
  ["not UTF-8", () => [file("bad.txt", new Uint8Array([0xff, 0xfe, 0x41]))], textOnly],
  ["NUL byte", () => [file("bin.txt", "a\0b")], textOnly],
  ["five files", () => Array.from({ length: 5 }, (_, i) => file(`${i}.txt`, "x")), textOnly],
  ["over the count with existing", () => [file("a.txt", "x")], textOnly, [1, 2, 3, 4].map((i) => ({ id: `${i}`, kind: "text", name: `${i}.txt`, size: 1, text: "x" }))],
  ["retained storage full", () => [file("a.png", png, "image/png")], vision, [], [{ id: "r", kind: "image", name: "r.png", size: 1, url: "x".repeat(16 * 1024 * 1024) }]],
];

export function readAttachments(impl: Impl, side: Side, [, files, m, existing = [], retained = []]: (typeof readCases)[number]) {
  return outcome(async () => noIds(await impl.readAttachments(files(), m[side], existing, retained)));
}

export const acceptModels = [textOnly, vision, model([])];

const image = { id: "i", kind: "image", name: "a.png", size: 10, url: "data:image/png;base64,AAAA" };
const text = { id: "t", kind: "text", name: "a.txt", size: 5, text: "hello" };
export const conversations: [string, () => any[]][] = [
  ["plain", () => [{ id: "1", role: "user", text: "hi" }]],
  ["image", () => [{ id: "1", role: "user", text: "see", attachments: [image] }]],
  ["text", () => [{ id: "1", role: "user", text: "", attachments: [text] }]],
  ["missing", () => [{ id: "1", role: "user", text: "x", attachmentsMissing: true }]],
  ["too large", () => [{ id: "1", role: "user", text: "x".repeat(21 * 1024 * 1024) }]],
];

// conversationChecks is each model's pair: the attachment error, and what chatRequestError returns
// or throws (as its message).
export function conversationChecks(impl: Impl, side: Side, messages: any[]) {
  return [textOnly, vision].map((m) => {
    let request;
    try { request = impl.chatRequestError(messages, m[side]); } catch (e) { request = (e as Error).message; }
    return { conversation: impl.conversationAttachmentError(messages, m[side]) ?? null, request: request ?? null };
  });
}
