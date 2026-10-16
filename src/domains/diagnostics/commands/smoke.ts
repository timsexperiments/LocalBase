import { z } from "zod";
import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { SmokeInput } from "../../app/commands/inputs";
import { smokeStepSchema } from "../../app/commands/results";
import { byId, isDecisionModel } from "../../../catalog";
import { createApiKey, deleteApiKey } from "../../../manager";
import { LOCALBASE_VERSION } from "../../../version";
import {
  getGatewayInstanceState,
  gatewayHealthUrl,
} from "../../service/ownership";

type SmokeStep = z.infer<typeof smokeStepSchema>;

/** Scopes the throwaway credential needs: one inference call and residency reads. */
const SMOKE_KEY_SCOPES = [
  "inference:chat",
  "inference:decision",
  "models:read",
] as const;
const SMOKE_KEY_NAME = "local-base smoke (temporary)";
const HEALTH_TIMEOUT_MS = 5_000;
const MAX_TOKENS = 4;

// Loose views: the smoke check reads a handful of fields and must not break
// when the gateway adds others.
const healthViewSchema = z.object({
  status: z.string(),
  version: z.string().optional(),
  modalities: z
    .object({
      llm: z.object({ configured: z.boolean(), state: z.string() }),
    })
    .optional(),
});

const metadataListViewSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      catalog: z.object({
        kind: z.string(),
        capabilities: z.object({ kind: z.string() }).nullable().optional(),
      }),
      device: z.object({
        runtime: z.object({ state: z.string() }).nullable(),
      }),
    }),
  ),
});

const chatViewSchema = z.object({
  choices: z
    .array(
      z.object({
        message: z
          .object({
            content: z.string().nullable().optional(),
            reasoning_content: z.string().nullable().optional(),
          })
          .optional(),
      }),
    )
    .min(1),
  usage: z
    .object({ completion_tokens: z.number().optional() })
    .optional()
    .nullable(),
});

// The smallest valid System One request asks one yes/no ("noul") question.
const DECISION_QUESTION_ID = "ok";

const decisionViewSchema = z.object({
  answers: z.record(
    z.string(),
    z.discriminatedUnion("type", [
      z.object({ type: z.literal("noul"), noul: z.number() }),
      z.object({ type: z.literal("choice"), choice: z.string() }),
      z.object({ type: z.literal("score"), score: z.number() }),
    ]),
  ),
});

const errorViewSchema = z.object({
  error: z.object({ message: z.string(), code: z.unknown().optional() }),
});

class SmokeFailure extends Error {}

async function errorReason(response: Response): Promise<string> {
  const text = await response.text().catch(() => "");
  let message = text.trim().slice(0, 300);
  try {
    const parsed = errorViewSchema.safeParse(JSON.parse(text));
    if (parsed.success) {
      const { code } = parsed.data.error;
      message =
        typeof code === "string"
          ? `${parsed.data.error.message} (${code})`
          : parsed.data.error.message;
    }
  } catch {
    // Not JSON; keep the trimmed body.
  }
  return `HTTP ${response.status}${message ? `: ${message}` : ""}`;
}

async function request(
  url: string,
  init: RequestInit,
  timeoutMs: number,
): Promise<Response> {
  try {
    return await fetch(url, {
      ...init,
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (error) {
    if (error instanceof Error && error.name === "TimeoutError")
      throw new SmokeFailure(`no response within ${timeoutMs / 1000}s`);
    throw new SmokeFailure(
      `request failed: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
}

async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    throw new SmokeFailure(`HTTP ${response.status} returned invalid JSON`);
  }
}

async function checkHealth(baseUrl: string): Promise<string> {
  const response = await request(
    `${baseUrl}/health`,
    { method: "GET" },
    HEALTH_TIMEOUT_MS,
  );
  if (!response.ok) throw new SmokeFailure(await errorReason(response));
  const health = healthViewSchema.safeParse(await readJson(response));
  if (!health.success)
    throw new SmokeFailure("health response has an unexpected shape");
  if (health.data.status !== "ok")
    throw new SmokeFailure(`gateway reports status ${health.data.status}`);
  const llm = health.data.modalities?.llm;
  if (!llm?.configured)
    throw new SmokeFailure("the gateway has no LLM configured");
  if (llm.state === "failed")
    throw new SmokeFailure("the LLM runtime is in the failed state");
  const version = health.data.version ?? "unknown";
  const mismatch =
    version === LOCALBASE_VERSION ? "" : ` (this CLI is ${LOCALBASE_VERSION})`;
  return `ok, version ${version}${mismatch}, llm ${llm.state}`;
}

type LlmView = Readonly<{
  /** The LLM the gateway currently holds a runtime for, if any. */
  resident: string | null;
  /** LLMs the gateway serves from POST /v1/systemone instead of chat. */
  decision: ReadonlySet<string>;
}>;

async function readLlms(
  baseUrl: string,
  headers: HeadersInit,
  timeoutMs: number,
): Promise<LlmView> {
  const response = await request(
    `${baseUrl}/_localbase/models`,
    { method: "GET", headers },
    timeoutMs,
  );
  if (!response.ok)
    throw new SmokeFailure(
      `could not read model residency: ${await errorReason(response)}`,
    );
  const list = metadataListViewSchema.safeParse(await readJson(response));
  if (!list.success)
    throw new SmokeFailure("model metadata has an unexpected shape");
  const llms = list.data.data.filter((model) => model.catalog.kind === "llm");
  return {
    resident: llms.find((model) => model.device.runtime !== null)?.id ?? null,
    decision: new Set(
      llms
        .filter((model) => model.catalog.capabilities?.kind === "decision")
        .map((model) => model.id),
    ),
  };
}

async function chat(
  baseUrl: string,
  headers: HeadersInit,
  model: string,
  timeoutMs: number,
): Promise<string> {
  const response = await request(
    `${baseUrl}/v1/chat/completions`,
    {
      method: "POST",
      headers: { ...headers, "content-type": "application/json" },
      body: JSON.stringify({
        model,
        messages: [{ role: "user", content: "Reply with OK." }],
        max_tokens: MAX_TOKENS,
        stream: false,
      }),
    },
    timeoutMs,
  );
  if (!response.ok) throw new SmokeFailure(await errorReason(response));
  const completion = chatViewSchema.safeParse(await readJson(response));
  if (!completion.success)
    throw new SmokeFailure("chat completion has no choices");
  const message = completion.data.choices[0]?.message;
  const text = message?.content || message?.reasoning_content || "";
  const tokens = completion.data.usage?.completion_tokens ?? 0;
  if (!text && tokens <= 0)
    throw new SmokeFailure("chat completion returned no output");
  return `completed with ${tokens} output token${tokens === 1 ? "" : "s"}`;
}

async function decide(
  baseUrl: string,
  headers: HeadersInit,
  model: string,
  timeoutMs: number,
): Promise<string> {
  const response = await request(
    `${baseUrl}/v1/systemone`,
    {
      method: "POST",
      headers: { ...headers, "content-type": "application/json" },
      body: JSON.stringify({
        model,
        state: "LocalBase smoke check.",
        questions: {
          [DECISION_QUESTION_ID]: {
            type: "noul",
            instructions: "Is this a test?",
          },
        },
      }),
    },
    timeoutMs,
  );
  if (!response.ok) throw new SmokeFailure(await errorReason(response));
  const decision = decisionViewSchema.safeParse(await readJson(response));
  const answer = decision.success
    ? decision.data.answers[DECISION_QUESTION_ID]
    : undefined;
  if (answer?.type !== "noul")
    throw new SmokeFailure("decision response has no answer to the question");
  return `answered a yes/no question (noul ${answer.noul.toFixed(2)})`;
}

/** Prefers the gateway's classification; the local catalog covers misses. */
function isDecision(model: string, llms: LlmView | null): boolean {
  return llms?.decision.has(model) || isDecisionModel(byId(model));
}

/** Sends the smallest real request the model serves: a chat or a decision. */
async function infer(
  baseUrl: string,
  headers: HeadersInit,
  model: string,
  timeoutMs: number,
  llms: LlmView | null,
): Promise<string> {
  return isDecision(model, llms)
    ? await decide(baseUrl, headers, model, timeoutMs)
    : await chat(baseUrl, headers, model, timeoutMs);
}

async function switchModel(
  baseUrl: string,
  headers: HeadersInit,
  from: string,
  to: string,
  timeoutMs: number,
  llms: LlmView | null,
): Promise<string> {
  let failure: SmokeFailure | undefined;
  try {
    await infer(baseUrl, headers, to, timeoutMs, llms);
  } catch (error) {
    if (!(error instanceof SmokeFailure)) throw error;
    failure = error;
  }
  const resident = await readLlms(baseUrl, headers, timeoutMs).then(
    (view) => view.resident,
    () => null,
  );
  if (failure) {
    const rolledBack = resident === from ? `; ${from} is still resident` : "";
    throw new SmokeFailure(`${failure.message}${rolledBack}`);
  }
  if (resident === from)
    throw new SmokeFailure(`switch was rolled back; ${from} is still resident`);
  if (resident !== to)
    throw new SmokeFailure(
      `switch did not land; resident model is ${resident ?? "none"}`,
    );
  return `${to} is resident and answered`;
}

export type SmokeOptions = {
  /** Overrides gateway discovery; tests point this at a fixture. */
  resolveBaseUrl?: (root: string) => Promise<string>;
};

async function discoverBaseUrl(root: string): Promise<string> {
  const state = await getGatewayInstanceState(root);
  if (state.state === "active")
    return gatewayHealthUrl(state.instance).replace(/\/health$/, "");
  const detail =
    state.state === "missing"
      ? "no gateway is running"
      : state.state === "stale"
        ? "the recorded gateway process is no longer running"
        : state.detail;
  throw new SmokeFailure(`${detail} for ${state.root}`);
}

export async function runSmoke(
  input: SmokeInput,
  ctx: AppContext,
  execution: CommandExecution,
  options: SmokeOptions = {},
) {
  const out = execution.output;
  const timeoutMs = input.timeout * 1000;
  const steps: SmokeStep[] = [];
  // Assigned inside step callbacks, so held in an object TS does not narrow.
  const run: { gatewayUrl: string | null; failure: string | null } = {
    gatewayUrl: null,
    failure: null,
  };

  const step = async (
    name: SmokeStep["name"],
    model: string | null,
    work: () => Promise<string>,
  ): Promise<void> => {
    const started = performance.now();
    let ok = true;
    let detail: string;
    try {
      detail = await work();
    } catch (error) {
      if (!(error instanceof SmokeFailure)) throw error;
      ok = false;
      detail = error.message;
    }
    const durationMs = Math.round(performance.now() - started);
    steps.push({ name, model, ok, durationMs, detail });
    const label = model ? `${name} ${model}` : name;
    if (ok) out.info(`PASS ${label}: ${detail} (${durationMs} ms)`);
    else out.error(`FAIL ${label}: ${detail}`);
    if (!ok) {
      run.failure = `${label}: ${detail}`;
      throw new SmokeFailure(detail);
    }
  };

  try {
    await step("health", null, async () => {
      const url = await (options.resolveBaseUrl ?? discoverBaseUrl)(
        ctx.config.root,
      );
      run.gatewayUrl = url;
      return `${await checkHealth(url)} at ${url}`;
    });
    const baseUrl = run.gatewayUrl!;
    // A throwaway key stands in for the user's: the CLI already owns the
    // root database the gateway authenticates against.
    const { record, rawKey } = createApiKey(
      ctx.database,
      ctx.config,
      SMOKE_KEY_NAME,
      1,
      [...SMOKE_KEY_SCOPES],
    );
    try {
      const headers = {
        authorization: `Bearer ${rawKey}`,
        "x-api-key": rawKey,
      };
      let llms: LlmView | null = null;
      try {
        llms = await readLlms(baseUrl, headers, timeoutMs);
      } catch {
        // Fall back to the configured default and the local catalog; the
        // inference step reports failures.
      }
      const model = llms?.resident ?? ctx.config.activeLlmModel;
      const inference = isDecision(model, llms) ? "decision" : "chat";
      if (!model) {
        await step(inference, null, async () => {
          throw new SmokeFailure("no resident or default LLM is configured");
        });
      }
      await step(inference, model, () =>
        infer(baseUrl, headers, model, timeoutMs, llms),
      );
      const target = input.switchTo;
      if (target) {
        await step("switch", target, async () => {
          if (target === model)
            throw new SmokeFailure(`${target} is already the resident model`);
          return await switchModel(
            baseUrl,
            headers,
            model,
            target,
            timeoutMs,
            llms,
          );
        });
        await step("switch-back", model, () =>
          switchModel(baseUrl, headers, target, model, timeoutMs, llms),
        );
      }
    } finally {
      deleteApiKey(ctx.database, ctx.config, record.id);
    }
  } catch (error) {
    if (!(error instanceof SmokeFailure)) throw error;
  }

  const { gatewayUrl, failure } = run;
  if (failure === null) out.info("Smoke check passed.");
  else out.error(`Smoke check failed: ${failure}`);
  return {
    data: { passed: failure === null, gatewayUrl, steps, failure },
    ...(failure === null ? {} : { exitCode: 1 }),
  };
}
