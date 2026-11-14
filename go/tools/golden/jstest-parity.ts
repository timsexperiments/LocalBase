// Freezes the TS side of the page-script parity tests (go/tools/jstest) as fixtures.
//
//   bun run go/tools/golden/jstest-parity.ts <commit>
//
// The Go page scripts were ported from the TS UI (src/ui), and six jstest files compared the two on the
// same inputs. The TS UI is being retired with src/, so this runs its side once, over the inputs in
// go/tools/jstest/parity/*.cases.ts, and writes go/tools/jstest/testdata/<module>.json. <commit> is the
// commit whose src/ it runs; it refuses if src/ here differs from it. Run before src/ is deleted; after
// that, the fixtures are reviewed by hand, like this directory's other outputs.
import { spawnSync } from "child_process";
import { writeFileSync } from "fs";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import Markdown from "react-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { gfm } from "micromark-extension-gfm";
import * as tsAttachments from "../../../src/ui/attachments";
import { appendDictation } from "../../../src/ui/dictation";
import { encodePcm16Wav } from "../../../src/ui/dictation-audio";
import { deltaReasoning, thinkSplitter } from "../../../src/ui/reasoning";
import { chatContext } from "../../../src/ui/compaction";
import { historyKey, readHistory, streamText, writeHistory } from "../../../src/ui/client";
import * as tsCompaction from "../../../src/ui/compaction";
import * as tsSteering from "../../../src/ui/steering";
import * as goChatCore from "../../internal/web/static/js/chat-core.js";
import * as compaction from "../jstest/parity/compaction.cases.ts";
import * as tsTools from "../../../src/ui/tools";
import * as tsSettings from "../../../src/ui/generation-settings";
import * as tsClient from "../../../src/ui/client";
import * as tools from "../jstest/parity/tools.cases.ts";
import * as attachments from "../jstest/parity/attachments.cases.ts";
import * as chatCore from "../jstest/parity/chat-core.cases.ts";
import * as dictation from "../jstest/parity/dictation.cases.ts";
import * as markdown from "../jstest/parity/markdown.cases.ts";
import { snap } from "../jstest/parity/frozen.ts";

const commit = process.argv[2];
if (!commit) throw new Error("usage: bun run go/tools/golden/jstest-parity.ts <commit>");
const root = new URL("../../../", import.meta.url).pathname;
const git = (...args: string[]) => spawnSync("git", args, { cwd: root, encoding: "utf8" });
const full = git("rev-parse", "--verify", `${commit}^{commit}`).stdout.trim();
if (!full) throw new Error(`no commit ${commit}`);
if (git("diff", "--quiet", full, "--", "src").status !== 0) throw new Error(`src/ here differs from ${commit}; run at that commit's src/`);

function write(name: string, test: string, data: Record<string, unknown>) {
  const out = {
    _frozen: {
      from: full.slice(0, 12),
      by: "go/tools/golden/jstest-parity.ts",
      note: `The TS UI's outputs for ${test}'s inputs, run once before the TypeScript UI was retired. Reviewed by hand from here on: a change here is a change to what the Go page does.`,
    },
    ...data,
  };
  writeFileSync(new URL(`../jstest/testdata/${name}.json`, import.meta.url), JSON.stringify(out, null, 1) + "\n");
  console.log(`wrote testdata/${name}.json`);
}

// attachments.test.ts
{
  const readAttachments: Record<string, unknown> = {};
  for (const c of attachments.readCases) readAttachments[c[0]] = await attachments.readAttachments(tsAttachments as any, "ts", c);
  const conversations: Record<string, unknown> = {};
  for (const [name, messages] of attachments.conversations) conversations[name] = snap(attachments.conversationChecks(tsAttachments as any, "ts", messages()));
  write("attachments", "attachments.test.ts", {
    readAttachments,
    accept: snap(attachments.acceptModels.map((m) => tsAttachments.attachmentAccept(m.ts))),
    conversations,
  });
}

// dictation.test.ts
{
  const appends: Record<string, string> = {};
  for (const [value, text] of dictation.appends) appends[JSON.stringify([value, text])] = appendDictation(value, text);
  const wavs: Record<string, unknown> = {};
  for (const [name, samples, capture] of dictation.wavs) wavs[name] = snap(await dictation.bytes(encodePcm16Wav(samples(), capture)));
  const refused: Record<string, string> = {};
  for (const [name, samples] of dictation.refused) {
    try {
      encodePcm16Wav(samples());
    } catch (e) {
      refused[name] = (e as Error).message;
    }
    if (!(name in refused)) throw new Error(`TS encodePcm16Wav accepted ${name}`);
  }
  write("dictation", "dictation.test.ts", { appendDictation: appends, encodePcm16Wav: wavs, refused });
}

// markdown.test.ts: react-markdown with GFM, as message-markdown.tsx configures it.
{
  function remarkGfm(this: any) {
    const data = this.data();
    (data.micromarkExtensions ??= []).push(gfm());
    (data.fromMarkdownExtensions ??= []).push(gfmFromMarkdown());
  }
  const html: Record<string, string> = {};
  for (const md of markdown.corpus) html[md] = markdown.norm(renderToStaticMarkup(createElement(Markdown, { remarkPlugins: [remarkGfm] }, md)));
  write("markdown", "markdown.test.ts", { html });
}

// chat-core.test.ts. TS's history functions read and write globalThis.localStorage.
{
  const g = globalThis as any;
  const splits: Record<string, unknown> = {};
  for (const text of chatCore.texts) splits[text] = chatCore.splits(thinkSplitter, text);
  const context: Record<string, unknown> = {};
  for (const [name, compaction] of chatCore.contextCases) context[name] = snap(chatContext(chatCore.conversation as any, compaction));

  const written = new chatCore.MemoryStorage();
  g.localStorage = written;
  writeHistory(chatCore.conversations as any);
  // What TS reads from the Go page's serialization (the history both now share).
  const fromGo = new chatCore.MemoryStorage();
  fromGo.setItem(goChatCore.historyKey, goChatCore.serializeHistory(chatCore.conversations));
  g.localStorage = fromGo;
  const read = snap(readHistory());
  g.localStorage = new chatCore.MemoryStorage();
  const savingOff = readHistory();
  const rejected = chatCore.invalid.map((raw) => {
    const s = new chatCore.MemoryStorage();
    s.setItem(historyKey, raw);
    g.localStorage = s;
    try {
      readHistory();
      return false;
    } catch {
      return true;
    }
  });
  const extra = new chatCore.MemoryStorage();
  extra.setItem(historyKey, chatCore.extraKeys);
  g.localStorage = extra;
  const extraKeys = snap(readHistory());
  delete g.localStorage;

  const streams: Record<string, unknown> = {};
  for (const [name, events] of Object.entries(chatCore.streams)) streams[name] = snap(await chatCore.runStream(streamText, events));
  write("chat-core", "chat-core.test.ts", {
    thinkSplitter: splits,
    deltaReasoning: snap(chatCore.deltas.map((d) => deltaReasoning(d as any))),
    chatContext: context,
    history: { key: historyKey, written: written.getItem(historyKey), read, savingOff, rejected, extraKeys },
    streamText: streams,
  });
}

// compaction.test.ts. TS's context windows live in globalThis.localStorage; compact posts with fetch.
{
  const g = globalThis as any;
  const c = compaction;
  const ts = tsCompaction;

  const storage = new c.MemoryStorage();
  g.localStorage = storage;
  const fresh = c.windowModels().map((m) => ts.contextWindow(m.ts as any));
  const learnFirst = ts.learnContext("a", 16384);
  const learnAgain = ts.learnContext("a", 16384);
  const learnedStorage = [...storage.items];
  const afterLearn = ts.contextWindow(c.windowModels()[0].ts as any);
  const key = learnedStorage[0]?.[0];
  if (!key) throw new Error("TS learnContext stored nothing");
  storage.setItem(key, c.damaged[0]);
  const damaged = ["a", "b", "c"].map((id) => ts.contextWindow(c.windowModel(id, 9000).ts as any));
  storage.setItem(key, c.damaged[1]);
  const notJSON = ts.contextWindow(c.windowModel("a").ts as any);
  delete g.localStorage;

  const planCompaction: Record<string, unknown> = {};
  for (const [name, options] of c.plans) planCompaction[name] = snap(ts.planCompaction(options()));

  const messages = c.compactMessages();
  const plan = ts.planCompaction({ messages: messages as any, budget: c.compactBudget })!;
  const cases: Record<string, unknown> = {};
  const fetch = g.fetch;
  for (const [name, content, withPrevious] of c.compactCases) {
    const bodies: any[] = [];
    g.fetch = async (_url: any, init: any) => (bodies.push(JSON.parse(init.body)), c.sse(content));
    const result = await ts.compact({ connection: { kind: "session" }, model: { id: "m" } as any, messages: messages as any, compaction: withPrevious ? c.previous : undefined,
      ...plan, context: 8192, signal: new AbortController().signal });
    cases[name] = { result: snap(result), body: snap(bodies[0]) };
  }
  g.fetch = fetch;

  write("compaction", "compaction.test.ts", {
    estimateTextTokens: c.textTokens.map((t) => ts.estimateTextTokens(t)),
    estimateTokens: ts.estimateTokens(c.tokenMessages as any),
    overflowContext: snap(c.overflows.map((m) => ts.overflowContext(m))),
    promptBudget: c.budgets.map(([context, max]) => ts.promptBudget(context, max)),
    contextWindow: { fresh, learnFirst, learnAgain, learnedStorage, afterLearn, damaged, notJSON },
    planCompaction,
    transcript: c.transcriptSizes.map((n) => ts.transcript(c.transcriptMessages as any, n)),
    compact: { plan: snap(plan), cases },
    phaseLabel: c.phases.map(([phase, s]) => tsSteering.phaseLabel(phase, s)),
    interruptReply: snap(c.interrupts.map(([m, label]) => tsSteering.interruptReply(m, label))),
  });
}

// tools.test.ts. The models are the TS schema's parse of each input, frozen so the test can use them.
{
  const t = tools;
  const models = t.buildModels((input) => tsClient.modelsSchema.parse(input).data[0]);
  const byId = (id: string) => models.find((m) => m.id === id);
  const defaults = tsSettings.defaultGenerationSettings();
  const tsRun = (common: any) => tsTools.runChat({ ...common, connection: { kind: "session" } } as any);
  const runChat: Record<string, unknown> = {};
  for (const [name, replies, options, gw] of t.chatCases(defaults)) runChat[name] = snap(await t.run(tsRun, models, replies, options, gw));
  const gatewayFetch: Record<string, unknown> = {};
  for (const [name, reply] of t.fetchCases)
    for (const method of t.fetchMethods) gatewayFetch[`${name} ${method}`] = await t.fetchOutcome(tsClient.gatewayFetch as any, reply, method);
  write("tools", "tools.test.ts", {
    models: snap(models),
    defaultGenerationSettings: snap(defaults),
    runChat,
    generationTools: snap(t.toolPermissions.map(([m, p]) => tsTools.generationTools(models, byId(m), p as any))),
    catalogModels: snap(t.catalogModes.map((mode) => tsClient.catalogModels(t.shuffledIds.map(byId), mode))),
    parameters: {
      chat: snap(t.chatParams.map((s) => t.paramOutcome(() => tsSettings.chatParameters(s as any)))),
      image: snap(t.imageParams.map(([s, m]) => t.paramOutcome(() => tsSettings.imageParameters(s as any, byId(m))))),
      video: snap(t.videoParams.map((s) => t.paramOutcome(() => tsSettings.videoParameters(s as any)))),
      speech: snap(t.speechParams.map(([m, s]) => t.paramOutcome(() => tsSettings.speechParameters(byId(m), s as any)))),
    },
    gatewayFetch,
  });
}
