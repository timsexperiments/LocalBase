// The Go UI's attachments against the TS attachments.ts they were ported from, frozen as fixtures
// (parity/frozen.ts; testdata/attachments.json).
//
//   bun test go/tools/jstest
import { describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/attachments.js";
import { acceptModels, conversationChecks, conversations, readAttachments, readCases } from "./parity/attachments.cases.ts";
import { fixture, snap } from "./parity/frozen.ts";

const want = fixture("attachments");

describe("readAttachments", () => {
  for (const c of readCases)
    test(c[0], async () => expect(await readAttachments(go as any, "go", c)).toEqual(want.readAttachments[c[0]]));
});

describe("checks", () => {
  test("accept", () => {
    expect(snap(acceptModels.map((m) => go.attachmentAccept(m.go)))).toEqual(want.accept);
  });
  for (const [name, messages] of conversations)
    test(name, () => expect(snap(conversationChecks(go as any, "go", messages()))).toEqual(want.conversations[name]));
});
