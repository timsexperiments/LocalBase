import { expect, test } from "bun:test";

const read = (name: string) => Bun.file(new URL(`../../internal/web/static/js/${name}`, import.meta.url)).text();
const personKeyLine = (text: string) => text.split("\n").filter((l) => l.includes("const personKey"));

// person.js repeats this one line rather than import chat-core.js, which would put Decisions over its asset budget.
// If they drifted apart, Chat and Studio would keep one person's things under two different keys.
test("the two copies of personKey are the same, byte for byte", async () => {
  const [person, chat] = await Promise.all([read("person.js"), read("chat-core.js")]);
  expect(personKeyLine(person).length).toBe(1);
  expect(personKeyLine(chat)).toEqual(personKeyLine(person));
});

test("Studio's keys are per person, and the shared ones are gone from what's read", async () => {
  const [lab, decisions] = await Promise.all([read("lab.js"), read("decisions.js")]);
  expect(lab).toContain("localbase.lab.fill.v2.${personKey()}");
  expect(lab).not.toContain('"localbase.lab.fill"');
  expect(decisions).toContain("localbase.decisions.v2.${personKey()}");
  expect(decisions).toContain("localbase.decisions.saved.v2.${personKey()}");
  expect(decisions).not.toContain('"localbase.decisions.v1"');
  expect(decisions).not.toMatch(/getItem\(OLD_SAVED\)\s*\?\?/);
});
