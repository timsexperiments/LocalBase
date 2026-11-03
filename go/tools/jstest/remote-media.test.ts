// The chat page sends an attached image itself, as a data: URL, which is the
// only form the gateway accepts.
//
//   bun test go/tools/jstest
import { expect, test } from "bun:test";
import { readAttachments } from "../../internal/web/static/js/attachments.js";
import { messageToChat } from "../../internal/web/static/js/chat-core.js";

const vision = { inputs: ["text", "image"] };
const images: [string, number[], string][] = [
  ["a.png", [137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13], "image/png"],
  ["a.jpg", [255, 216, 255, 224, 0, 16], "image/jpeg"],
];

for (const [name, bytes, mime] of images) {
  test(`${name} reaches the request as a data: URL`, async () => {
    const attachments = await readAttachments([new File([new Uint8Array(bytes)], name, { type: mime })], vision, []);
    const [message] = messageToChat({ id: "1", role: "user", text: "see", attachments });
    const parts = (message.content as any[]).filter((part) => part.type === "image_url");
    expect(parts.length).toBe(1);
    expect(parts[0].image_url.url.startsWith(`data:${mime};base64,`)).toBe(true);
  });
}
