// Fixtures frozen from the TypeScript UI the Go page scripts were ported from.
//
// Each parity test once ran the Go module and the TS function on the same inputs and compared them.
// The TS UI is retired, so go/tools/golden/jstest-parity.ts ran its side once, over the same inputs
// (the *.cases.ts files here), and wrote the outputs to ../testdata/*.json. The tests now compare the
// Go module's output with those. The fixtures are reviewed by hand from then on, like the outputs of
// the other go/tools/golden scripts: a change to one is a change to what the Go page does.

import { readFileSync } from "fs";

// snap is a value as a fixture holds it: plain JSON, with bytes as base64 and undefined left out, the
// same way for the Go output and for the frozen TS output, so the two compare with toEqual.
export function snap(v: unknown): any {
  return JSON.parse(JSON.stringify({ v: encode(v) })).v ?? null;
}

function encode(v: unknown): unknown {
  if (v instanceof Uint8Array) return { $bytes: Buffer.from(v).toString("base64") };
  if (Array.isArray(v)) return v.map(encode);
  if (v && typeof v === "object" && Object.getPrototypeOf(v) === Object.prototype)
    return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, encode(x)]));
  return v;
}

// outcome is what a call gives back, or the message it threw with.
export async function outcome(fn: () => unknown | Promise<unknown>): Promise<any> {
  try {
    return { ok: snap(await fn()) };
  } catch (e) {
    return { error: (e as Error).message };
  }
}

// fixture reads one module's frozen outputs.
export function fixture(name: string): any {
  const data = JSON.parse(readFileSync(new URL(`../testdata/${name}.json`, import.meta.url), "utf8"));
  if (!data._frozen?.from) throw new Error(`${name}.json has no _frozen header`);
  return data;
}
