// The Go UI's generation tools against TS tools.ts and generation-settings.tsx.
// Both run against the same scripted gateway, with deterministic ids and
// blob URLs, and must send the same requests and produce the same results.
//
//   bun test go/tools/jstest
import { afterEach, describe, expect, test } from "bun:test";
import * as go from "../../internal/web/static/js/tools.js";
import * as ts from "../../../src/ui/tools";
import * as settings from "../../../src/ui/generation-settings";
import { catalogModels, modelsSchema, type Model } from "../../../src/ui/client";
import * as tsClient from "../../../src/ui/client";

function fixture(id: string, kind: string, capabilities: any = null, features = ["tool-calling"]): Model {
  return modelsSchema.parse({
    host: { memory: { kind: "unified", system: { capacityBytes: 16 * 1024 ** 3, availableBytes: 8 * 1024 ** 3 }, accelerators: [] } },
    data: [{
      id,
      catalog: { name: id, kind, quantization: "test", qualificationState: "qualified", memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
        features, inputModalities: ["text"], outputModalities: ["text"], contextWindowTokens: null, capabilities },
      device: { selected: true, installed: true, runtime: null },
    }],
  }).data[0]!;
}
const chat = fixture("chat", "llm");
const plain = fixture("plain", "llm", null, []);
const image = fixture("image", "image");
const lora = fixture("lora", "image", { kind: "image", lora: { defaultStrength: 0.8, minimum: 0, maximum: 2, trigger: null } });
const speech = fixture("speech", "tts", { kind: "speech", voice: { requestValues: ["default", "harbor"], defaultRequestValue: "default" } });
const video = fixture("video", "video", { kind: "video", mode: "t2v", width: 256, height: 256, frames: 9, fps: 16, jobDeadlineMs: 5000, outputFormats: ["mp4"] });
const s2v = fixture("s2v", "video", { kind: "video", mode: "s2v", width: 256, height: 256, frames: 9, fps: 16, jobDeadlineMs: 5000, outputFormats: ["mp4"] });
const uninstalled = { ...image, id: "image2", device: { ...image.device, installed: false } } as Model;
const models = [chat, plain, image, lora, speech, video, s2v, uninstalled];
const all = ["inference:image", "inference:video", "inference:speech"];

const sse = (deltas: any[]) =>
  new Response(deltas.map((d) => `data: ${JSON.stringify({ choices: [{ delta: d.delta ?? {}, finish_reason: d.finish ?? null }] })}\n\n`).join("") + "data: [DONE]\n\n",
    { headers: { "content-type": "text/event-stream" } });
const call = (name: string, args: string) => [{ delta: { tool_calls: [{ index: 0, id: "x", type: "function", function: { name, arguments: args } }] } }, { finish: "tool_calls" }];
const text = (t: string) => [{ delta: { content: t } }, { finish: "stop" }];
const jobId = "6f1c2b9a-1d2e-4f3a-8b4c-5d6e7f8a9b0c";

// A scripted gateway: chat replies are consumed in order; media endpoints answer directly.
function gateway(replies: any[][], opts: { status?: number; imageBody?: any } = {}) {
  const requests: any[] = [];
  let i = 0;
  const fetch = async (input: any, init: any = {}) => {
    const url = String(input);
    const method = (init.method ?? "GET").toUpperCase();
    const body = init.body ? JSON.parse(init.body) : undefined;
    requests.push({ url, method, body });
    if (opts.status) return new Response(JSON.stringify({ error: { message: "nope", code: "x" } }), { status: opts.status, headers: { "content-type": "application/json" } });
    if (url.endsWith("/v1/chat/completions")) return sse(replies[i++] ?? text("done"));
    if (url.endsWith("/v1/images/generations")) return Response.json(opts.imageBody ?? { data: [{ b64_json: btoa("png") }] });
    if (url.endsWith("/v1/audio/speech")) return new Response("wav", { headers: { "content-type": "audio/wav" } });
    if (url.endsWith("/v1/videos")) return Response.json({ id: jobId, status: "completed" });
    if (url.endsWith("/content")) return new Response("mp4", { headers: { "content-type": "video/mp4" } });
    return Response.json({ id: jobId, status: "completed" });
  };
  return { fetch, requests };
}

const original = { fetch: globalThis.fetch, uuid: crypto.randomUUID.bind(crypto), url: URL.createObjectURL };
afterEach(() => {
  globalThis.fetch = original.fetch;
  crypto.randomUUID = original.uuid;
  URL.createObjectURL = original.url;
});
function deterministic() {
  let n = 0, u = 0;
  crypto.randomUUID = (() => `${String(++n).padStart(8, "0")}-0000-4000-8000-000000000000`) as any;
  URL.createObjectURL = (() => `blob:test/${++u}`) as any;
}

async function run(impl: "go" | "ts", replies: any[][], options: any = {}, gw: any = {}) {
  deterministic();
  const g = gateway(replies, gw);
  globalThis.fetch = g.fetch as any;
  const out: any = { text: "", artifacts: [] as any[], warnings: [] as string[] };
  const common = {
    preferences: options.preferences ?? {}, model: options.model ?? chat, models, signal: new AbortController().signal,
    messages: options.messages ?? [{ role: "user", content: "hi" }], toolsEnabled: options.toolsEnabled ?? true,
    permissions: options.permissions ?? all, append: (t: string) => (out.text += t), reasoning: () => {},
    artifact: (a: any) => out.artifacts.push(a), register: () => {}, warning: (w: string) => out.warnings.push(w),
  };
  try {
    out.result = impl === "go" ? await go.runChat(common) : await ts.runChat({ ...common, connection: { kind: "session" } } as any);
  } catch (e) {
    out.error = (e as Error).message;
    out.session = (e as Error).name === "SessionRequiredError" || e?.constructor?.name === "SessionRequiredError";
  }
  out.requests = g.requests;
  return out;
}

describe("runChat", () => {
  const cases: [string, any[][], any?, any?][] = [
    ["plain reply, no tool model", [text("Hello there")], { model: plain }],
    ["tools disabled", [text("Hi")], { toolsEnabled: false }],
    ["no permissions", [text("Hi")], { permissions: [] }],
    ["image", [call("generate_image", '{"model":"image","prompt":"  a cat  "}'), text("Here it is.")]],
    ["image with settings and LoRA", [call("generate_image", '{"model":"lora","prompt":"a dog"}'), text("Done")],
      { preferences: { lora: { ...settings.defaultGenerationSettings(), image: { size: "768x512", seed: 7, steps: 20, cfg_scale: 4.5, negative_prompt: "blur", lora_strength: 1.5, n: 3, strength: 0.4 } }, chat: { ...settings.defaultGenerationSettings(), llm: { temperature: 0.3, max_tokens: 99 } } } }],
    ["speech", [call("synthesize_speech", '{"model":"speech","input":"Hello"}'), text("Listen.")], { preferences: { speech: { ...settings.defaultGenerationSettings(), tts: { voice: "harbor" } } } }],
    ["speech with an unknown voice", [call("synthesize_speech", '{"model":"speech","input":"Hello"}'), text("Sorry.")], { preferences: { speech: { ...settings.defaultGenerationSettings(), tts: { voice: "nope" } } } }],
    ["video", [call("generate_video", '{"model":"video","prompt":"waves"}'), text("Watch.")]],
    ["s2v is not offered", [call("generate_video", '{"model":"s2v","prompt":"waves"}'), text("No.")]],
    ["uninstalled model", [call("generate_image", '{"model":"image2","prompt":"x"}'), text("No.")]],
    ["malformed JSON", [call("generate_image", "{nope"), text("Oops.")]],
    ["extra argument", [call("generate_image", '{"model":"image","prompt":"x","n":2}'), text("Oops.")]],
    ["blank prompt", [call("generate_image", '{"model":"image","prompt":"   "}'), text("Oops.")]],
    ["speech too long", [call("synthesize_speech", `{"model":"speech","input":"${"x".repeat(257)}"}`), text("Oops.")]],
    ["unknown tool", [call("delete_files", "{}"), text("No.")]],
    ["tool without permission", [call("generate_video", '{"model":"video","prompt":"x"}'), text("No.")], { permissions: ["inference:image"] }],
    ["tool call without tools", [call("generate_image", '{"model":"image","prompt":"x"}')], { model: plain }],
    ["round limit", Array.from({ length: 5 }, () => call("generate_image", '{"model":"image","prompt":"x"}'))],
    ["empty image response", [call("generate_image", '{"model":"image","prompt":"x"}'), text("Hm.")], {}, { imageBody: { data: [] } }],
    ["prior tool ids are not reused", [call("generate_image", '{"model":"image","prompt":"x"}'), text("ok")],
      { messages: [{ role: "assistant", content: null, tool_calls: [{ id: "000000010", type: "function", function: { name: "generate_image", arguments: "{}" } }] }, { role: "tool", tool_call_id: "000000010", content: "{}" }, { role: "user", content: "again" }] }],
    ["gateway error", [text("x")], {}, { status: 503 }],
    ["session ended", [text("x")], {}, { status: 401 }],
  ];
  for (const [name, replies, options, gw] of cases)
    test(name, async () => {
      const want = await run("ts", replies, options, gw);
      const got = await run("go", replies, options, gw);
      if (want.session) {
        expect(got.session).toBe(true);
        delete want.error, delete got.error;
      }
      expect(got).toEqual(want);
    }, 20_000);
});

describe("settings", () => {
  test("generationTools", () => {
    for (const [m, p] of [[chat, all], [chat, ["inference:speech"]], [plain, all], [chat, []]] as const)
      expect(go.generationTools(models, m, p)).toEqual(ts.generationTools(models, m, p as any));
  });
  test("catalog order", () => {
    const shuffled = [uninstalled, s2v, video, speech, lora, image, plain, chat];
    for (const mode of ["llm", "image", "video", "tts"] as const) expect(go.catalogModels(shuffled, mode)).toEqual(catalogModels(shuffled, mode));
  });
  const outcome = (f: () => any) => { try { return { ok: f() }; } catch { return { error: true }; } };
  test("parameters", () => {
    for (const s of [{}, { temperature: 0.7 }, { max_tokens: 5 }, { temperature: 3 }, { max_tokens: 0 }, { max_tokens: 1.5 }])
      expect(outcome(() => go.chatParameters(s))).toEqual(outcome(() => settings.chatParameters(s as any)));
    for (const [s, m] of [[{ size: "512x768", seed: 1 }, image], [{ size: "9x9" }, image], [{ steps: 0 }, image], [{ cfg_scale: 31 }, image], [{ seed: -1 }, image],
      [{ lora_strength: 2 }, lora], [{ lora_strength: 2.1 }, lora], [{ lora_strength: 1 }, image], [{ strength: 0.5, n: 2 }, image]] as const)
      expect(outcome(() => go.imageParameters(s, m))).toEqual(outcome(() => settings.imageParameters(s as any, m)));
    for (const s of [{}, { negative_prompt: "x" }, { negative_prompt: "x".repeat(16385) }])
      expect(outcome(() => go.videoParameters(s))).toEqual(outcome(() => settings.videoParameters(s as any)));
    for (const [m, s] of [[speech, {}], [speech, { voice: "harbor" }], [speech, { voice: "x" }], [image, {}]] as const)
      expect(outcome(() => go.speechParameters(m, s))).toEqual(outcome(() => settings.speechParameters(m, s as any)));
  });
});

describe("gatewayFetch", () => {
  const outcome = async (f: () => Promise<Response>) => {
    try {
      const r = await f();
      return { status: r.status };
    } catch (e) {
      return { error: (e as Error).message, name: (e as Error).name };
    }
  };
  const cases: [string, () => Promise<Response>][] = [
    ["ok", async () => new Response("{}", { status: 200 })],
    ["edge redirect", async () => new Response(null, { status: 302, headers: { location: "https://team.cloudflareaccess.com/login" } })],
    ["unreachable", async () => { throw new TypeError("Load failed"); }],
  ];
  for (const [name, reply] of cases)
    for (const method of ["GET", "POST"])
      test(`${name} ${method}`, async () => {
        globalThis.fetch = reply as any;
        const want = await outcome(() => tsClient.gatewayFetch("/x", { method }));
        const got = await outcome(() => go.gatewayFetch("/x", { method }));
        expect(got).toEqual(want);
      });
});
