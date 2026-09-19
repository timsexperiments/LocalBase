import { expect, test } from "bun:test";
import {
  attachmentAccept,
  attachmentError,
  messageToChat,
  readAttachments,
  userMessageContent,
  type Attachment,
} from "./attachments";
import { modelsSchema } from "./client";

function model(inputModalities = ["text"]) {
  return modelsSchema.parse({
    host: {
      memory: {
        kind: "unified",
        system: {
          capacityBytes: 16 * 1024 ** 3,
          availableBytes: 8 * 1024 ** 3,
        },
        accelerators: [],
      },
    },
    data: [
      {
        id: "chat",
        catalog: {
          name: "Chat",
          kind: "llm",
          quantization: "Q4",
          memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
          inputModalities,
          outputModalities: ["text"],
          contextWindowTokens: 4096,
          capabilities: null,
        },
        device: { selected: true, installed: true, runtime: null },
      },
    ],
  }).data[0];
}

test("reads text and code files without changing their contents", async () => {
  const files = [
    new File(["name,value\nalpha,42\n"], "data.csv"),
    new File(["print('hello')"], "script.py"),
  ];
  const attachments = await readAttachments(files, model(), []);
  expect(attachments.map(({ name, size }) => ({ name, size }))).toEqual(
    files.map(({ name, size }) => ({ name, size })),
  );
  expect(userMessageContent("Summarize", attachments)).toEqual([
    { type: "text", text: "Summarize" },
    { type: "text", text: 'Attached file "data.csv":\nname,value\nalpha,42\n' },
    { type: "text", text: "Attached file \"script.py\":\nprint('hello')" },
  ]);
  expect(attachmentAccept(model())).toContain(".csv");
  expect(attachmentAccept(model())).not.toContain("image/");
  expect(attachmentAccept(undefined)).toBe("");
});

test("images require advertised vision and preserve supported media type", async () => {
  const png = new File(
    [Uint8Array.from([137, 80, 78, 71, 13, 10, 26, 10])],
    "sample.png",
    { type: "image/png" },
  );
  await expect(readAttachments([png], model(), [])).rejects.toThrow(
    "does not support image",
  );
  const attachments = await readAttachments(
    [png],
    model(["text", "image"]),
    [],
  );
  expect(attachmentAccept(model(["text", "image"]))).toContain("image/png");
  expect(userMessageContent("", attachments)).toEqual([
    {
      type: "image_url",
      image_url: { url: "data:image/png;base64,iVBORw0KGgo=" },
    },
  ]);
  expect(attachmentError(attachments, model())).toContain("supports image");
  await expect(
    readAttachments([new File(["not png"], "bad.png")], model(["image"]), []),
  ).rejects.toThrow("PNG, JPEG, or WebP");
});

test("rejects unsupported documents, binary text, empty and oversized files", async () => {
  for (const file of [
    new File(["%PDF"], "file.pdf"),
    new File(["PK"], "file.docx"),
  ]) {
    await expect(readAttachments([file], model(), [])).rejects.toThrow(
      "not supported",
    );
  }
  await expect(
    readAttachments(
      [new File([new Uint8Array([255])], "bad.txt")],
      model(),
      [],
    ),
  ).rejects.toThrow("UTF-8");
  await expect(
    readAttachments([new File(["\0"], "bad.txt")], model(), []),
  ).rejects.toThrow("binary");
  await expect(
    readAttachments([new File([], "empty.txt")], model(), []),
  ).rejects.toThrow("nonempty");
  await expect(
    readAttachments(
      [new File([new Uint8Array(128 * 1024 + 1)], "large.txt")],
      model(),
      [],
    ),
  ).rejects.toThrow("128 KiB");
  await expect(
    readAttachments(
      [new File([new Uint8Array(5 * 1024 * 1024 + 1)], "large.png")],
      model(["image"]),
      [],
    ),
  ).rejects.toThrow("5 MiB");
});

test("enforces count and aggregate limits when appending to an existing draft", async () => {
  const attachment: Attachment = {
    id: "one",
    kind: "text",
    name: "one.txt",
    size: 128 * 1024,
    text: "content",
  };
  await expect(
    readAttachments([new File(["extra"], "extra.txt")], model(), [
      attachment,
      attachment,
      attachment,
      attachment,
    ]),
  ).rejects.toThrow("4 files");
  await expect(
    readAttachments([new File(["extra"], "extra.txt")], model(), [
      attachment,
      attachment,
    ]),
  ).rejects.toThrow("256 KiB");
  expect(attachmentError([attachment], undefined)).toContain("Choose a model");
});

test("follow-ups and retries retain attachments and assistant tool protocol", () => {
  expect(() =>
    messageToChat({
      id: "restored",
      role: "user",
      text: "",
      attachmentsMissing: true,
    }),
  ).toThrow("missing its attachments");
  const attachment: Attachment = {
    id: "one",
    kind: "text",
    name: "one.txt",
    size: 7,
    text: "content",
  };
  const user = {
    id: "user",
    role: "user" as const,
    text: "",
    attachments: [attachment],
  };
  expect(messageToChat(user)).toEqual([
    { role: "user", content: userMessageContent("", [attachment]) },
  ]);
  const protocol = [{ role: "assistant" as const, content: "answer" }];
  expect(
    messageToChat({
      id: "assistant",
      role: "assistant",
      text: "answer",
      protocol,
    }),
  ).toBe(protocol);
  expect(messageToChat({ id: "plain", role: "user", text: "Hello" })).toEqual([
    { role: "user", content: "Hello" },
  ]);
});
