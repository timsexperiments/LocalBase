import { expect, test } from "bun:test";
import type { HubTokenCheck } from "./huggingface";
import {
  hubTokenRequestSchema,
  hubTokenStatus,
  runHubTokenRequest,
  type HubTokenDependencies,
} from "./hub-token";

const TOKEN = "hf_" + "a".repeat(34);
const OTHER = "hf_" + "b".repeat(34);

function deps(
  options: { saved?: string; env?: string; check?: HubTokenCheck } = {},
) {
  let saved = options.saved ?? "";
  const checked: string[] = [];
  const value: HubTokenDependencies = {
    saved: () => saved || undefined,
    env: () => options.env,
    save: async (token) => {
      saved = token;
    },
    check: async (token) => {
      checked.push(token);
      return options.check ?? { state: "valid", name: "someone", role: "read" };
    },
  };
  return { value, checked, saved: () => saved };
}

test("reports whether a token is set and where it comes from", () => {
  expect(hubTokenStatus(deps().value)).toEqual({
    configured: false,
    source: null,
    envAvailable: false,
  });
  expect(hubTokenStatus(deps({ env: OTHER }).value)).toEqual({
    configured: true,
    source: "env",
    envAvailable: true,
  });
  expect(hubTokenStatus(deps({ saved: TOKEN, env: OTHER }).value)).toEqual({
    configured: true,
    source: "saved",
    envAvailable: true,
  });
});

test("saves after a successful check and never returns the token", async () => {
  const f = deps();
  const outcome = await runHubTokenRequest(f.value, {
    action: "save",
    token: TOKEN,
  });
  expect(outcome.status).toBe(200);
  expect(f.saved()).toBe(TOKEN);
  expect(f.checked).toEqual([TOKEN]);
  expect(outcome.body).toMatchObject({
    configured: true,
    source: "saved",
    check: { state: "valid" },
  });
  expect(JSON.stringify(outcome)).not.toContain(TOKEN);
});

test("refuses to save a token the Hub rejects", async () => {
  const f = deps({ saved: OTHER, check: { state: "invalid" } });
  const outcome = await runHubTokenRequest(f.value, {
    action: "save",
    token: TOKEN,
  });
  expect(outcome.status).toBe(400);
  expect(outcome.body).toMatchObject({ error: { code: "hf_token_invalid" } });
  expect(f.saved()).toBe(OTHER);
  expect(JSON.stringify(outcome)).not.toContain(TOKEN);
});

test("saves when the Hub cannot be reached, or when checking is skipped", async () => {
  const offline = deps({
    check: { state: "unreachable", message: "Could not reach Hugging Face." },
  });
  const outcome = await runHubTokenRequest(offline.value, {
    action: "save",
    token: TOKEN,
  });
  expect(outcome.status).toBe(200);
  expect(outcome.body).toMatchObject({ check: { state: "unreachable" } });
  expect(offline.saved()).toBe(TOKEN);

  const unchecked = deps();
  const skipped = await runHubTokenRequest(unchecked.value, {
    action: "save",
    token: TOKEN,
    validate: false,
  });
  expect(skipped.body).not.toHaveProperty("check");
  expect(unchecked.checked).toEqual([]);
  expect(unchecked.saved()).toBe(TOKEN);
});

test("clearing falls back to HF_TOKEN", async () => {
  const f = deps({ saved: TOKEN, env: OTHER });
  const outcome = await runHubTokenRequest(f.value, { action: "clear" });
  expect(f.saved()).toBe("");
  expect(outcome.body).toEqual({
    configured: true,
    source: "env",
    envAvailable: true,
  });
});

test("validates the token in use, saved before env", async () => {
  const f = deps({ saved: TOKEN, env: OTHER });
  const outcome = await runHubTokenRequest(f.value, { action: "validate" });
  expect(f.checked).toEqual([TOKEN]);
  expect(outcome.body).toMatchObject({ check: { state: "valid" } });
  const missing = await runHubTokenRequest(deps().value, {
    action: "validate",
  });
  expect(missing.status).toBe(409);
  expect(missing.body).toMatchObject({ error: { code: "hf_token_missing" } });
});

test("accepts only bounded, space-free tokens and strict bodies", () => {
  expect(
    hubTokenRequestSchema.parse({ action: "save", token: ` ${TOKEN}\n` }),
  ).toEqual({ action: "save", token: TOKEN });
  for (const body of [
    { action: "save", token: "short" },
    { action: "save", token: "hf_ has spaces in it" },
    { action: "save", token: "x".repeat(257) },
    { action: "save" },
    { action: "clear", token: TOKEN },
    { action: "reveal" },
  ])
    expect(hubTokenRequestSchema.safeParse(body).success).toBe(false);
});
