// The Go UI's generation tools against the TS tools.ts and generation-settings.tsx they were ported
// from, frozen as fixtures (parity/frozen.ts; testdata/tools.json). Each runs against the same
// scripted gateway, with deterministic ids and blob URLs, and must send the same requests and produce
// the same results.
//
//   bun test go/tools/jstest
import { afterEach, describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/tools.js";
import * as c from "./parity/tools.cases.ts";
import { fixture, snap } from "./parity/frozen.ts";

const want = fixture("tools");
const models: any[] = want.models;
const byId = (id: string) => models.find((m) => m.id === id);
afterEach(c.restore);

// The one deliberate difference from TS (DESIGN §24): with no seed set, the Go
// tool sends a random one. A chosen seed still has to match below.
function expectRandomSeeds(got: any, frozen: any) {
  got.requests.forEach((r: any, i: number) => {
    if (!r.url.endsWith("/v1/images/generations") || frozen.requests[i]?.body?.seed !== undefined) return;
    expect(Number.isInteger(r.body.seed) && r.body.seed >= 0 && r.body.seed <= 2147483647).toBe(true);
    delete r.body.seed;
  });
}

describe("runChat", () => {
  for (const [name, replies, options, gw] of c.chatCases(want.defaultGenerationSettings))
    test(name, async () => {
      const frozen = structuredClone(want.runChat[name]);
      const got = snap(await c.run(go.runChat, models, replies, options, gw));
      expectRandomSeeds(got, frozen);
      if (frozen.session) {
        expect(got.session).toBe(true);
        delete frozen.error, delete got.error;
      }
      expect(got).toEqual(frozen);
    }, 20_000);
});

test("a chat image with no seed gets a random one", async () => {
  const seeds = new Set<number>();
  for (let i = 0; i < 8; i++) {
    const got = await c.run(go.runChat, models, [c.call("generate_image", '{"model":"image","prompt":"x"}'), c.text("ok")]);
    seeds.add(got.requests.find((r: any) => r.url.endsWith("/v1/images/generations")).body.seed);
  }
  expect(seeds.size).toBeGreaterThan(1);
});

describe("settings", () => {
  test("generationTools", () => {
    expect(snap(c.toolPermissions.map(([m, p]) => go.generationTools(models, byId(m), p)))).toEqual(want.generationTools);
  });
  test("catalog order", () => {
    const shuffled = c.shuffledIds.map(byId);
    expect(snap(c.catalogModes.map((mode) => go.catalogModels(shuffled, mode)))).toEqual(want.catalogModels);
  });
  test("parameters", () => {
    expect(snap(c.chatParams.map((s) => c.paramOutcome(() => go.chatParameters(s))))).toEqual(want.parameters.chat);
    expect(snap(c.imageParams.map(([s, m]) => c.paramOutcome(() => go.imageParameters(s, byId(m)))))).toEqual(want.parameters.image);
    expect(snap(c.videoParams.map((s) => c.paramOutcome(() => go.videoParameters(s))))).toEqual(want.parameters.video);
    expect(snap(c.speechParams.map(([m, s]) => c.paramOutcome(() => go.speechParameters(byId(m), s))))).toEqual(want.parameters.speech);
  });
});

describe("gatewayFetch", () => {
  for (const [name, reply] of c.fetchCases)
    for (const method of c.fetchMethods)
      test(`${name} ${method}`, async () => {
        expect(await c.fetchOutcome(go.gatewayFetch, reply, method)).toEqual(want.gatewayFetch[`${name} ${method}`]);
      });
});
