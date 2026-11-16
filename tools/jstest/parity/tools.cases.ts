// The inputs of tools.test.ts, shared by the test (the Go side) and the fixture generator (the frozen
// TS side). See frozen.ts. The models were built with the TS UI's model schema, and the preferences
// from its default settings; the generator freezes both, so the test needs neither.

// modelInput is what the TS schema parsed into a model.
export function modelInput(id: string, kind: string, capabilities: any = null, features = ["tool-calling"]) {
  return {
    host: { memory: { kind: "unified", system: { capacityBytes: 16 * 1024 ** 3, availableBytes: 8 * 1024 ** 3 }, accelerators: [] } },
    data: [{
      id,
      catalog: { name: id, kind, quantization: "test", qualificationState: "qualified", memory: { minimumVramEstimateGb: 1, storageEstimateGb: 1 },
        features, inputModalities: ["text"], outputModalities: ["text"], contextWindowTokens: null, capabilities },
      device: { selected: true, installed: true, runtime: null },
    }],
  };
}

export const modelInputs: [string, string, any?, string[]?][] = [
  ["chat", "llm"],
  ["plain", "llm", null, []],
  ["image", "image"],
  ["lora", "image", { kind: "image", lora: { defaultStrength: 0.8, minimum: 0, maximum: 2, trigger: null } }],
  ["speech", "tts", { kind: "speech", voice: { requestValues: ["default", "harbor"], defaultRequestValue: "default" } }],
  ["video", "video", { kind: "video", mode: "t2v", width: 256, height: 256, frames: 9, fps: 16, jobDeadlineMs: 5000, outputFormats: ["mp4"] }],
  ["s2v", "video", { kind: "video", mode: "s2v", width: 256, height: 256, frames: 9, fps: 16, jobDeadlineMs: 5000, outputFormats: ["mp4"] }],
];

// buildModels parses each input, and adds image2: the image model, not installed.
export function buildModels(parse: (input: any) => any) {
  const list = modelInputs.map(([id, kind, capabilities, features]) => parse(modelInput(id, kind, capabilities, features)));
  const image = list.find((m) => m.id === "image");
  return [...list, { ...image, id: "image2", device: { ...image.device, installed: false } }];
}

export const all = ["inference:image", "inference:video", "inference:speech"];

const sse = (deltas: any[]) =>
  new Response(deltas.map((d) => `data: ${JSON.stringify({ choices: [{ delta: d.delta ?? {}, finish_reason: d.finish ?? null }] })}\n\n`).join("") + "data: [DONE]\n\n",
    { headers: { "content-type": "text/event-stream" } });
export const call = (name: string, args: string) => [{ delta: { tool_calls: [{ index: 0, id: "x", type: "function", function: { name, arguments: args } }] } }, { finish: "tool_calls" }];
export const text = (t: string) => [{ delta: { content: t } }, { finish: "stop" }];
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
export function restore() {
  globalThis.fetch = original.fetch;
  crypto.randomUUID = original.uuid;
  URL.createObjectURL = original.url;
}
function deterministic() {
  let n = 0, u = 0;
  crypto.randomUUID = (() => `${String(++n).padStart(8, "0")}-0000-4000-8000-000000000000`) as any;
  URL.createObjectURL = (() => `blob:test/${++u}`) as any;
}

// run is one chat turn through runChat (Go's, or TS's given a session connection), against the
// scripted gateway: what it said, made and warned of, its result or error, and every request.
export async function run(runChat: (common: any) => Promise<any>, models: any[], replies: any[][], options: any = {}, gw: any = {}) {
  deterministic();
  const g = gateway(replies, gw);
  globalThis.fetch = g.fetch as any;
  const byId = (id: string) => models.find((m) => m.id === id);
  const out: any = { text: "", artifacts: [] as any[], warnings: [] as string[] };
  const common = {
    preferences: options.preferences ?? {}, model: byId(options.model ?? "chat"), models, signal: new AbortController().signal,
    messages: options.messages ?? [{ role: "user", content: "hi" }], toolsEnabled: options.toolsEnabled ?? true,
    permissions: options.permissions ?? all, append: (t: string) => (out.text += t), reasoning: () => {},
    artifact: (a: any) => out.artifacts.push(a), register: () => {}, warning: (w: string) => out.warnings.push(w),
  };
  try {
    out.result = await runChat(common);
  } catch (e) {
    out.error = (e as Error).message;
    out.session = (e as Error).name === "SessionRequiredError" || (e as any)?.constructor?.name === "SessionRequiredError";
  }
  out.requests = g.requests;
  restore();
  return out;
}

// chatCases are the runChat cases; d is the default generation settings, for preferences.
export const chatCases = (d: any): [string, any[][], any?, any?][] => [
  ["plain reply, no tool model", [text("Hello there")], { model: "plain" }],
  ["tools disabled", [text("Hi")], { toolsEnabled: false }],
  ["no permissions", [text("Hi")], { permissions: [] }],
  ["image", [call("generate_image", '{"model":"image","prompt":"  a cat  "}'), text("Here it is.")]],
  ["image with settings and LoRA", [call("generate_image", '{"model":"lora","prompt":"a dog"}'), text("Done")],
    { preferences: { lora: { ...d, image: { size: "768x512", seed: 7, steps: 20, cfg_scale: 4.5, negative_prompt: "blur", lora_strength: 1.5, n: 3, strength: 0.4 } }, chat: { ...d, llm: { temperature: 0.3, max_tokens: 99 } } } }],
  ["speech", [call("synthesize_speech", '{"model":"speech","input":"Hello"}'), text("Listen.")], { preferences: { speech: { ...d, tts: { voice: "harbor" } } } }],
  ["speech with an unknown voice", [call("synthesize_speech", '{"model":"speech","input":"Hello"}'), text("Sorry.")], { preferences: { speech: { ...d, tts: { voice: "nope" } } } }],
  ["video", [call("generate_video", '{"model":"video","prompt":"waves"}'), text("Watch.")]],
  ["s2v is not offered", [call("generate_video", '{"model":"s2v","prompt":"waves"}'), text("No.")]],
  ["uninstalled model", [call("generate_image", '{"model":"image2","prompt":"x"}'), text("No.")]],
  ["malformed JSON", [call("generate_image", "{nope"), text("Oops.")]],
  ["extra argument", [call("generate_image", '{"model":"image","prompt":"x","n":2}'), text("Oops.")]],
  ["blank prompt", [call("generate_image", '{"model":"image","prompt":"   "}'), text("Oops.")]],
  ["speech too long", [call("synthesize_speech", `{"model":"speech","input":"${"x".repeat(257)}"}`), text("Oops.")]],
  ["unknown tool", [call("delete_files", "{}"), text("No.")]],
  ["tool without permission", [call("generate_video", '{"model":"video","prompt":"x"}'), text("No.")], { permissions: ["inference:image"] }],
  ["tool call without tools", [call("generate_image", '{"model":"image","prompt":"x"}')], { model: "plain" }],
  ["round limit", Array.from({ length: 5 }, () => call("generate_image", '{"model":"image","prompt":"x"}'))],
  ["empty image response", [call("generate_image", '{"model":"image","prompt":"x"}'), text("Hm.")], {}, { imageBody: { data: [] } }],
  ["prior tool ids are not reused", [call("generate_image", '{"model":"image","prompt":"x"}'), text("ok")],
    { messages: [{ role: "assistant", content: null, tool_calls: [{ id: "000000010", type: "function", function: { name: "generate_image", arguments: "{}" } }] }, { role: "tool", tool_call_id: "000000010", content: "{}" }, { role: "user", content: "again" }] }],
  ["gateway error", [text("x")], {}, { status: 503 }],
  ["session ended", [text("x")], {}, { status: 401 }],
];

// The settings checks, by model id.
export const toolPermissions: [string, string[]][] = [["chat", all], ["chat", ["inference:speech"]], ["plain", all], ["chat", []]];
export const shuffledIds = ["image2", "s2v", "video", "speech", "lora", "image", "plain", "chat"];
export const catalogModes = ["llm", "image", "video", "tts"] as const;
export const chatParams = [{}, { temperature: 0.7 }, { max_tokens: 5 }, { temperature: 3 }, { max_tokens: 0 }, { max_tokens: 1.5 }];
export const imageParams: [any, string][] = [[{ size: "512x768", seed: 1 }, "image"], [{ size: "9x9" }, "image"], [{ steps: 0 }, "image"], [{ cfg_scale: 31 }, "image"], [{ seed: -1 }, "image"],
  [{ lora_strength: 2 }, "lora"], [{ lora_strength: 2.1 }, "lora"], [{ lora_strength: 1 }, "image"], [{ strength: 0.5, n: 2 }, "image"]];
export const videoParams = [{}, { negative_prompt: "x" }, { negative_prompt: "x".repeat(16385) }];
export const speechParams: [string, any][] = [["speech", {}], ["speech", { voice: "harbor" }], ["speech", { voice: "x" }], ["image", {}]];
export const paramOutcome = (f: () => any) => { try { return { ok: f() }; } catch { return { error: true }; } };

export const fetchCases: [string, () => Promise<Response>][] = [
  ["ok", async () => new Response("{}", { status: 200 })],
  ["edge redirect", async () => new Response(null, { status: 302, headers: { location: "https://team.cloudflareaccess.com/login" } })],
  ["unreachable", async () => { throw new TypeError("Load failed"); }],
];
export const fetchMethods = ["GET", "POST"];
export async function fetchOutcome(gatewayFetch: (url: string, init: any) => Promise<Response>, reply: () => Promise<Response>, method: string) {
  globalThis.fetch = reply as any;
  try {
    const r = await gatewayFetch("/x", { method });
    return { status: r.status };
  } catch (e) {
    return { error: (e as Error).message, name: (e as Error).name };
  } finally {
    restore();
  }
}
