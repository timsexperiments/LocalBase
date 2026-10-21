// Golden declarative configuration from the TypeScript code.
//
//   bun run go/tools/golden/config.ts > go/internal/conf/testdata/declarative.json
//
// The rendered default configuration, then documents derived from it: each
// one's validation message or normalized configuration, and its plan
// against the default configuration (and against nothing saved).
import { defaultConfig } from "../../../src/manager";
import {
  configurationDocument,
  parseConfiguration,
  planConfiguration,
  renderConfiguration,
} from "../../../src/domains/config/declarative";

const current = defaultConfig("/r", 0);
const base = renderConfiguration(configurationDocument(current));

function edit(...pairs: [RegExp, string][]): string {
  let doc = base;
  for (const [pattern, replacement] of pairs) {
    if (!pattern.test(doc)) throw new Error(`${pattern} does not match`);
    doc = doc.replace(pattern, replacement);
  }
  return doc;
}

const tts = /^selectedTtsModels = \[\]$/m;
const documents: Record<string, string> = {
  base,
  change: edit([/^port = 2273$/m, "port = 2274"], [/^percent = 15$/m, "percent = 12.5"],
    [/^ctxSize = 131072$/m, "ctxSize = 65536"], [/^parallel = "auto"$/m, "parallel = 2"]),
  hot: edit([/^ctxSize = 131072$/m, "ctxSize = 32768"], [tts, 'selectedTtsModels = ["qwen3-tts-1.7b-base-q4_k_m"]']),
  unsorted: edit([/^selectedLlmModels = .*$/m, 'selectedLlmModels = ["qwen2.5-coder-7b-instruct-q4_k_m", "clef-flash-q4_k_m"]']),
  floats: edit([/^port = 2273$/m, "port = 2273.0"], [/^parallel = "auto"$/m, "parallel = 3.0"], [/^minimumGb = 8$/m, "minimumGb = 0.25"]),
  noAllow: edit([/^allowExperimental = false\n/m, ""]),
  allowed: edit([/^allowExperimental = false$/m, "allowExperimental = true"], [tts, 'selectedTtsModels = ["kokoro-82m-v1.0-q8"]']),
  syntax: "version = 1\nthis is not toml = \n",
  v2: edit([/^version = 1$/m, "version = 2"]),
  vstring: edit([/^version = 1$/m, 'version = "1"']),
  only: "version = 1\n",
  bad1: 'version = 1\n[gateway]\nhost = "a b"\nport = 0\nextra = 1\n',
  ws: edit([/^host = "127.0.0.1"$/gm, 'host = " x"']),
  tab: edit([/^sttHost = "127.0.0.1"$/m, 'sttHost = "a\\tb"']),
  empty: edit([/^host = "127.0.0.1"$/gm, 'host = ""']),
  long: edit([/^host = "127.0.0.1"$/gm, `host = "${"a".repeat(254)}"`]),
  hostnum: edit([/^host = "127.0.0.1"$/gm, "host = 5"]),
  port0: edit([/^port = 2273$/m, "port = 0"]),
  portbig: edit([/^port = 2273$/m, "port = 70000"]),
  portfloat: edit([/^port = 18000$/m, "port = 18000.5"]),
  portstr: edit([/^port = 18000$/m, 'port = "18000"']),
  portbool: edit([/^sttPort = 18080$/m, "sttPort = true"]),
  extra: edit([/^port = 2273$/m, 'port = 2273\nzzz = 1\naaa = 2\n"1" = 3']),
  rootextra: edit([/^version = 1$/m, "version = 1\nextra = true"]),
  subtable: edit([/^\[gateway\]$/m, "[gateway.sub]\nx = 1\n[gateway]"]),
  ctxmode: edit([/^ctxMode = "auto"$/m, 'ctxMode = "fast"']),
  noctxmode: edit([/^ctxMode = "auto"\n/m, ""]),
  par5: edit([/^parallel = "auto"$/m, "parallel = 5"]),
  parstr: edit([/^parallel = "auto"$/m, 'parallel = "2"']),
  ctxsmall: edit([/^ctxSize = 131072$/m, "ctxSize = 1024"]),
  ctxbig: edit([/^ctxSize = 131072$/m, "ctxSize = 3000000000"]),
  pct: edit([/^percent = 15$/m, "percent = 101"]),
  pctinf: edit([/^percent = 15$/m, "percent = inf"]),
  pctnan: edit([/^percent = 15$/m, "percent = nan"]),
  minneg: edit([/^minimumGb = 8$/m, "minimumGb = -1"]),
  memextra: edit([/^\[memory\]$/m, "[memory]\nfoo = 1"]),
  memarray: edit([/^\[memory\.systemReserve\]\npercent = 15\nminimumGb = 8\n/m, "systemReserve = [1]\n"]),
  missing: edit([/\[memory\.acceleratorReserve\][^]*/m, ""]),
  allowexp: edit([/^allowExperimental = false$/m, 'allowExperimental = "yes"']),
  allownum: edit([/^allowExperimental = false$/m, "allowExperimental = 1"], [tts, 'selectedTtsModels = ["nope"]']),
  modelsextra: edit([/^allowExperimental = false$/m, "allowExperimental = false\nzzz = 1"], [tts, 'selectedTtsModels = ["nope"]']),
  emptyid: edit([tts, 'selectedTtsModels = [""]']),
  listtype: edit([tts, 'selectedTtsModels = "x"']),
  itemtype: edit([tts, "selectedTtsModels = [1]"]),
  dup: edit([tts, 'selectedTtsModels = ["qwen3-tts-1.7b-base-q4_k_m", "qwen3-tts-1.7b-base-q4_k_m"]']),
  experimental: edit([tts, 'selectedTtsModels = ["kokoro-82m-v1.0-q8"]']),
  activeExperimental: edit([tts, 'selectedTtsModels = ["kokoro-82m-v1.0-q8"]'], [/^activeTtsModel = ""$/m, 'activeTtsModel = "kokoro-82m-v1.0-q8"']),
  wrongKind: edit([tts, 'selectedTtsModels = ["whisper-base-en-q8_0"]']),
  badmodels: edit([/^selectedLlmModels = \["(.*)"\]$/m, 'selectedLlmModels = ["nope", "$1"]'],
    [/^activeSttModel = ".*"$/m, 'activeSttModel = "qwen2.5-coder-7b-instruct-q4_k_m"']),
  noactive: edit([/^activeLlmModel = ".*"$/m, 'activeLlmModel = ""'], [tts, 'selectedTtsModels = ["nope"]']),
  activenum: edit([/^activeSttModel = ".*"$/m, "activeSttModel = 3"]),
  nollm: edit([/^selectedLlmModels = .*$/m, "selectedLlmModels = []"]),
  activetts: edit([/^activeTtsModel = ""$/m, 'activeTtsModel = "nope"']),
  video: edit([/^selectedVideoModels = \[\]$/m, 'selectedVideoModels = ["nope", "nope"]'], [/^activeVideoModel = ""$/m, 'activeVideoModel = "nope"']),
};

const settle = <T>(run: () => T) => {
  try {
    return { ok: run() };
  } catch (error) {
    return { error: (error as Error).message };
  }
};

const cases = Object.entries(documents).map(([name, doc]) => {
  const parsed = settle(() => parseConfiguration(doc));
  return {
    name,
    doc,
    ...("ok" in parsed
      ? {
          configuration: parsed.ok,
          plan: settle(() => planConfiguration(current, parsed.ok, false)),
          planPending: settle(() => planConfiguration(current, parsed.ok, true)),
          planFresh: settle(() => planConfiguration(undefined, parsed.ok, false)),
          rendered: renderConfiguration(parsed.ok),
        }
      : { error: parsed.error }),
  };
});
console.log(JSON.stringify({ base, cases }, null, 1));
