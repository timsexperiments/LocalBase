/**
 * Saves, replaces, clears and checks the Hugging Face token for the admin
 * UI. The token travels only inbound (save) and to huggingface.co (check);
 * no outcome ever carries any part of it.
 */
import { z } from "zod";
import type { HubTokenCheck } from "./huggingface";

/** Printable ASCII without spaces; Hub tokens are `hf_` plus ~34 chars. */
const tokenSchema = z
  .string()
  .trim()
  .regex(/^[\x21-\x7e]{8,256}$/, "expected a Hugging Face access token");

export const hubTokenRequestSchema = z.discriminatedUnion("action", [
  z
    .object({
      action: z.literal("save"),
      token: tokenSchema,
      validate: z.boolean().optional(),
    })
    .strict(),
  z.object({ action: z.literal("clear") }).strict(),
  z.object({ action: z.literal("validate") }).strict(),
]);
export type HubTokenRequest = z.infer<typeof hubTokenRequestSchema>;

export type HubTokenSource = "saved" | "env";

export type HubTokenStatus = Readonly<{
  configured: boolean;
  /** Where the token in use comes from; a saved token wins over HF_TOKEN. */
  source: HubTokenSource | null;
  /** True when HF_TOKEN is set, so clearing the saved token falls back. */
  envAvailable: boolean;
}>;

export type HubTokenOutcome =
  | Readonly<{
      status: 200;
      body: HubTokenStatus & Readonly<{ check?: HubTokenCheck }>;
    }>
  | Readonly<{
      status: 400 | 409;
      body: Readonly<{ error: { message: string; code: string } }>;
    }>;

export type HubTokenDependencies = Readonly<{
  /** The saved token, "" or undefined when none is saved. */
  saved: () => string | undefined;
  /** HF_TOKEN from the gateway environment. */
  env: () => string | undefined;
  /** Persists the token; "" clears it. */
  save: (token: string) => Promise<void>;
  check: (token: string) => Promise<HubTokenCheck>;
}>;

export function hubTokenStatus(
  deps: Pick<HubTokenDependencies, "saved" | "env">,
): HubTokenStatus {
  const saved = !!deps.saved();
  const env = !!deps.env();
  return {
    configured: saved || env,
    source: saved ? "saved" : env ? "env" : null,
    envAvailable: env,
  };
}

export async function runHubTokenRequest(
  deps: HubTokenDependencies,
  request: HubTokenRequest,
): Promise<HubTokenOutcome> {
  if (request.action === "clear") {
    await deps.save("");
    return { status: 200, body: hubTokenStatus(deps) };
  }
  if (request.action === "validate") {
    const token = deps.saved() || deps.env();
    if (!token)
      return {
        status: 409,
        body: {
          error: {
            message: "No Hugging Face token is saved or set in HF_TOKEN.",
            code: "hf_token_missing",
          },
        },
      };
    return {
      status: 200,
      body: { ...hubTokenStatus(deps), check: await deps.check(token) },
    };
  }
  const check =
    request.validate === false ? undefined : await deps.check(request.token);
  if (check?.state === "invalid")
    return {
      status: 400,
      body: {
        error: {
          message:
            "Hugging Face rejected that token, so it was not saved. Copy a fresh read token from huggingface.co/settings/tokens.",
          code: "hf_token_invalid",
        },
      },
    };
  await deps.save(request.token);
  return {
    status: 200,
    body: { ...hubTokenStatus(deps), ...(check ? { check } : {}) },
  };
}
