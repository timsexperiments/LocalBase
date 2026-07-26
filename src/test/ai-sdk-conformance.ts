import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
import type { GatewayFixture } from "./gateway-fixture";

export function createLocalBaseAiSdkProvider(
  gateway: GatewayFixture,
  apiKey?: string,
) {
  return createOpenAICompatible({
    baseURL: `${gateway.baseUrl}/v1`,
    name: "localbase-conformance",
    apiKey,
    includeUsage: true,
  });
}

export function latestUpstreamRequestBody(
  gateway: GatewayFixture,
): Record<string, unknown> {
  const body = gateway.upstreamRequests.at(-1)?.body;
  if (!body) throw new Error("Expected an upstream request.");
  return JSON.parse(body) as Record<string, unknown>;
}
