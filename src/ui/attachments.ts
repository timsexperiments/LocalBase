import {
  type ChatMessage,
  type Message,
  type Model,
  type UserContentPart,
} from "./client";

export type Attachment = { id: string; name: string; size: number } & (
  { kind: "text"; text: string } | { kind: "image"; url: string }
);

const textExtensions = [
  "txt",
  "md",
  "csv",
  "tsv",
  "json",
  "jsonl",
  "yaml",
  "yml",
  "xml",
  "log",
  "js",
  "jsx",
  "ts",
  "tsx",
  "py",
  "go",
  "rs",
  "java",
  "c",
  "h",
  "cpp",
  "css",
  "html",
  "sql",
  "sh",
  "toml",
];
const imageTypes = ["image/png", "image/jpeg", "image/webp"];
export const attachmentLimits = {
  count: 4,
  imageBytes: 5 * 1024 * 1024,
  textBytes: 128 * 1024,
  totalTextBytes: 256 * 1024,
};

function supports(model: Model | undefined, kind: "text" | "image") {
  return (
    model?.catalog.kind === "llm" &&
    model.catalog.capabilities?.kind !== "embedding" &&
    model.catalog.inputModalities.includes(kind)
  );
}

export function attachmentAccept(model: Model | undefined): string {
  return [
    ...(supports(model, "text")
      ? textExtensions.map((extension) => `.${extension}`)
      : []),
    ...(supports(model, "image") ? imageTypes : []),
  ].join(",");
}

export function attachmentError(
  attachments: Attachment[],
  model: Model | undefined,
): string | null {
  if (attachments.length > attachmentLimits.count)
    return "Attach up to 4 files per message.";
  for (const attachment of attachments) {
    if (!supports(model, attachment.kind))
      return `Choose a model that supports ${attachment.kind} inputs, or remove ${attachment.name}.`;
  }
  if (
    attachments.reduce(
      (bytes, attachment) =>
        bytes + (attachment.kind === "text" ? attachment.size : 0),
      0,
    ) > attachmentLimits.totalTextBytes
  )
    return "Text attachments must total 256 KiB or less.";
  return null;
}

function imageMime(bytes: Uint8Array): string | null {
  if (
    [137, 80, 78, 71, 13, 10, 26, 10].every(
      (byte, index) => bytes[index] === byte,
    )
  )
    return "image/png";
  if (bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255)
    return "image/jpeg";
  if (
    String.fromCharCode(...bytes.slice(0, 4)) === "RIFF" &&
    String.fromCharCode(...bytes.slice(8, 12)) === "WEBP"
  )
    return "image/webp";
  return null;
}

export async function readAttachments(
  files: File[],
  model: Model,
  existing: Attachment[],
): Promise<Attachment[]> {
  if (files.length + existing.length > attachmentLimits.count)
    throw new Error("Attach up to 4 files per message.");
  const added: Attachment[] = [];
  for (const file of files) {
    const extension = file.name.split(".").at(-1)?.toLowerCase() ?? "";
    const image =
      imageTypes.includes(file.type) ||
      ["png", "jpg", "jpeg", "webp"].includes(extension);
    const kind = image ? "image" : "text";
    if (!supports(model, kind))
      throw new Error(`This model does not support ${kind} inputs.`);
    if (!image && !textExtensions.includes(extension))
      throw new Error(
        "Choose a UTF-8 text or code file. PDF, Office documents, and archives are not supported.",
      );
    const limit = image
      ? attachmentLimits.imageBytes
      : attachmentLimits.textBytes;
    if (file.size === 0 || file.size > limit)
      throw new Error(
        `${file.name}: choose a nonempty file under ${image ? "5 MiB" : "128 KiB"}.`,
      );
    const bytes = new Uint8Array(await file.arrayBuffer());
    const identity = {
      id: crypto.randomUUID(),
      name: file.name,
      size: file.size,
    };
    if (image) {
      const mime = imageMime(bytes);
      if (!mime)
        throw new Error(`${file.name}: choose a PNG, JPEG, or WebP image.`);
      let binary = "";
      for (let offset = 0; offset < bytes.length; offset += 8192)
        binary += String.fromCharCode(...bytes.subarray(offset, offset + 8192));
      added.push({
        ...identity,
        kind: "image",
        url: `data:${mime};base64,${btoa(binary)}`,
      });
    } else {
      let text: string;
      try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
      } catch {
        throw new Error(`${file.name}: the file must contain UTF-8 text.`);
      }
      if (text.includes("\0"))
        throw new Error(`${file.name}: binary files are not supported.`);
      added.push({ ...identity, kind: "text", text });
    }
    const error = attachmentError([...existing, ...added], model);
    if (error) throw new Error(error);
  }
  return added;
}

export function userMessageContent(
  text: string,
  attachments: Attachment[],
): string | UserContentPart[] {
  if (!attachments.length) return text;
  const parts: UserContentPart[] = text ? [{ type: "text", text }] : [];
  for (const attachment of attachments) {
    parts.push(
      attachment.kind === "image"
        ? { type: "image_url", image_url: { url: attachment.url } }
        : {
            type: "text",
            text: `Attached file ${JSON.stringify(attachment.name)}:\n${attachment.text}`,
          },
    );
  }
  return parts;
}

export function messageToChat(message: Message): ChatMessage[] {
  if (message.attachmentsMissing)
    throw new Error(
      "This saved conversation is missing its attachments. Start a new conversation and attach the files again.",
    );
  if (message.protocol) return message.protocol;
  if (message.role === "user")
    return message.text || message.attachments?.length
      ? [
          {
            role: "user",
            content: userMessageContent(
              message.text,
              message.attachments ?? [],
            ),
          },
        ]
      : [];
  return message.text ? [{ role: "assistant", content: message.text }] : [];
}
