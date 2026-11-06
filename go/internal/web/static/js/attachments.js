// Chat attachments (TS attachments.ts): UTF-8 text and code files for every
// chat model, PNG/JPEG/WebP images for models that accept images. Files are
// read in the browser and sent inline; history never stores them. A model
// here is the page's chat model JSON: {inputs: ["text", "image", …]}.
import { messageToChat } from "./chat-core.js";

const textExtensions = ["txt", "md", "csv", "tsv", "json", "jsonl", "yaml", "yml", "xml", "log", "js", "jsx", "ts", "tsx", "py", "go", "rs", "java", "c", "h", "cpp", "css", "html", "sql", "sh", "toml"];
const imageTypes = ["image/png", "image/jpeg", "image/webp"];
export const attachmentLimits = {
  count: 4,
  imageBytes: 5 * 1024 * 1024,
  textBytes: 128 * 1024,
  totalTextBytes: 256 * 1024,
  conversationBytes: 16 * 1024 * 1024,
  requestBytes: 20 * 1024 * 1024,
};

const encoder = new TextEncoder();
const encodedBytes = (value) => encoder.encode(value).byteLength;
const attachmentBytes = (a) => encodedBytes(a.name) + (a.kind === "image" ? a.url.length : a.size);

export function attachmentStorageError(attachments) {
  if (attachments.reduce((bytes, a) => bytes + attachmentBytes(a), 0) > attachmentLimits.conversationBytes)
    return "Attachments open in this browser must total 16 MiB or less. Delete an attachment conversation, reload the page, or remove draft files.";
  return null;
}

const supports = (model, kind) => !!model?.inputs?.includes(kind);

export function attachmentAccept(model) {
  return [...(supports(model, "text") ? textExtensions.map((e) => `.${e}`) : []), ...(supports(model, "image") ? imageTypes : [])].join(",");
}

export function attachmentError(attachments, model) {
  if (attachments.length > attachmentLimits.count) return "Attach up to 4 files per message.";
  for (const a of attachments)
    if (!supports(model, a.kind)) return `Choose a model that supports ${a.kind} inputs, or remove ${a.name}.`;
  if (attachments.reduce((bytes, a) => bytes + (a.kind === "text" ? a.size : 0), 0) > attachmentLimits.totalTextBytes)
    return "Text attachments must total 256 KiB or less.";
  return null;
}

export function conversationAttachmentError(messages, model) {
  const retainedError = attachmentStorageError(messages.flatMap((m) => m.attachments ?? []));
  if (retainedError) return retainedError;
  for (const message of messages) {
    const error = attachmentError(message.attachments ?? [], model);
    if (error) return error;
  }
  if (messages.some((m) => m.attachmentsMissing))
    return "This saved conversation is missing attachments from an earlier session. Start a new conversation to send messages or retry.";
  return null;
}

export function chatRequestError(messages, model) {
  const problem = conversationAttachmentError(messages, model);
  if (problem) return problem;
  if (encodedBytes(JSON.stringify(messages.flatMap(messageToChat))) > attachmentLimits.requestBytes)
    return "This conversation is too large to send. Start a new conversation with a shorter prompt or fewer attachments.";
  return null;
}

function imageMime(bytes) {
  if ([137, 80, 78, 71, 13, 10, 26, 10].every((byte, i) => bytes[i] === byte)) return "image/png";
  if (bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) return "image/jpeg";
  if (String.fromCharCode(...bytes.slice(0, 4)) === "RIFF" && String.fromCharCode(...bytes.slice(8, 12)) === "WEBP") return "image/webp";
  return null;
}

/** Reads chosen files into attachments, or throws the reason they can't be used. */
export async function readAttachments(files, model, existing, retained = []) {
  if (files.length + existing.length > attachmentLimits.count) throw new Error("Attach up to 4 files per message.");
  const added = [];
  for (const file of files) {
    const extension = file.name.split(".").at(-1)?.toLowerCase() ?? "";
    const image = imageTypes.includes(file.type) || ["png", "jpg", "jpeg", "webp"].includes(extension);
    const kind = image ? "image" : "text";
    if (!supports(model, kind)) throw new Error(`This model does not support ${kind} inputs.`);
    if (!image && !textExtensions.includes(extension))
      throw new Error("Choose a UTF-8 text or code file. PDF, Office documents, and archives are not supported.");
    const limit = image ? attachmentLimits.imageBytes : attachmentLimits.textBytes;
    if (file.size === 0 || file.size > limit) throw new Error(`${file.name}: choose a nonempty file under ${image ? "5 MiB" : "128 KiB"}.`);
    const bytes = new Uint8Array(await file.arrayBuffer());
    const identity = { id: crypto.randomUUID(), name: file.name, size: file.size };
    if (image) {
      const mime = imageMime(bytes);
      if (!mime) throw new Error(`${file.name}: choose a PNG, JPEG, or WebP image.`);
      let binary = "";
      for (let offset = 0; offset < bytes.length; offset += 8192) binary += String.fromCharCode(...bytes.subarray(offset, offset + 8192));
      added.push({ ...identity, kind: "image", url: `data:${mime};base64,${btoa(binary)}` });
    } else {
      let text;
      try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
      } catch {
        throw new Error(`${file.name}: the file must contain UTF-8 text.`);
      }
      if (text.includes("\0")) throw new Error(`${file.name}: binary files are not supported.`);
      added.push({ ...identity, kind: "text", text });
    }
    const error = attachmentError([...existing, ...added], model);
    if (error) throw new Error(error);
    const retainedError = attachmentStorageError([...retained, ...existing, ...added]);
    if (retainedError) throw new Error(retainedError);
  }
  return added;
}
