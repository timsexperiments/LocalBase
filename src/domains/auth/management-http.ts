import type { DatabaseSession } from "../../db/client";
import {
  createApiKey,
  loadApiKeys,
  revokeApiKey,
  rotateApiKey,
  setApiKeyScopes,
  type LocalBaseConfig,
} from "../../manager";
import { authorize, type Permission, type Principal } from "./authorization";
import {
  disableBrowserAccess,
  loadBrowserAccessConfig,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
} from "./browser-access";
import { evaluateBrowserAccessPolicy } from "./browser-policy";
import { publicApiKey } from "./api-key-public";
import {
  accessManagementMutationResponseSchema,
  accessManagementReadResponseSchema,
  accessManagementRequestSchema,
  keyManagementMutationResponseSchema,
  keyManagementReadResponseSchema,
  keyManagementRequestSchema,
  managementErrorSchema,
} from "./management-contract";

const accessPath = "/_localbase/access-management";
const keysPath = "/_localbase/api-keys";
const maximumBodyBytes = 64 * 1_024;

function denied(status: 401 | 403): Response {
  return Response.json(
    managementErrorSchema.parse({
      error: {
        code: status === 401 ? "invalid_api_key" : "insufficient_permissions",
      },
    }),
    { status, headers: { "cache-control": "no-store" } },
  );
}

function permits(principal: Principal, permission: Permission): boolean {
  return (
    authorize({ principal, requirement: { kind: "permission", permission } })
      .kind === "authorized"
  );
}

async function boundedJson(request: Request): Promise<unknown> {
  const contentLength = request.headers.get("content-length");
  if (contentLength !== null) {
    const declared = Number(contentLength);
    if (!Number.isSafeInteger(declared) || declared < 0)
      throw new Error("invalid_length");
    if (declared > maximumBodyBytes) throw new Error("too_large");
  }
  const reader = request.body?.getReader();
  if (!reader) return null;
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maximumBodyBytes) throw new Error("too_large");
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  if (request.signal.aborted) throw new Error("aborted");
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return JSON.parse(new TextDecoder().decode(body));
}

export function createAuthManagement({
  root,
  database,
  configuration,
}: {
  root: string;
  database: DatabaseSession;
  configuration: () => LocalBaseConfig;
}) {
  const headers = { "cache-control": "no-store" };
  return async function handle(
    request: Request,
    principal: Principal,
  ): Promise<Response | null> {
    const pathname = new URL(request.url).pathname;
    if (pathname !== accessPath && pathname !== keysPath) return null;
    if (principal.kind === "anonymous") return denied(401);

    if (request.method === "GET") {
      const permission = pathname === accessPath ? "access:read" : "keys:read";
      if (!permits(principal, permission)) return denied(403);
      if (pathname === accessPath) {
        const config = await loadBrowserAccessConfig(root);
        return Response.json(
          accessManagementReadResponseSchema.parse({
            config: config ? summarizeBrowserAccessConfig(config) : null,
          }),
          { headers },
        );
      }
      return Response.json(
        keyManagementReadResponseSchema.parse({
          keys: loadApiKeys(database, configuration()).map(publicApiKey),
        }),
        { headers },
      );
    }
    if (request.method !== "POST")
      return new Response(null, {
        status: 405,
        headers: { allow: "GET, POST" },
      });

    let value: unknown;
    try {
      value = await boundedJson(request);
    } catch (error) {
      const code =
        error instanceof Error && error.message === "too_large"
          ? "payload_too_large"
          : request.signal.aborted ||
              (error instanceof Error && error.message === "aborted")
            ? "request_aborted"
            : "validation_failed";
      return Response.json(managementErrorSchema.parse({ error: { code } }), {
        status:
          code === "payload_too_large"
            ? 413
            : code === "request_aborted"
              ? 499
              : 400,
        headers,
      });
    }

    if (pathname === accessPath) {
      const parsed = accessManagementRequestSchema.safeParse(value);
      if (!parsed.success)
        return Response.json(
          managementErrorSchema.parse({
            error: { code: "validation_failed" },
          }),
          { status: 400, headers },
        );
      const input = parsed.data;
      const required =
        input.action === "test-policy" ? "access:read" : "access:manage";
      if (!permits(principal, required)) return denied(403);
      const current = await loadBrowserAccessConfig(root);
      switch (input.action) {
        case "configure-cloudflare":
        case "configure-oidc": {
          const config = await saveBrowserAccessConfig(root, {
            provider: input.provider,
            origin: input.origin,
            permissions: input.permissions,
            ...(current?.policy ? { policy: current.policy } : {}),
          });
          return Response.json(
            accessManagementMutationResponseSchema.parse({
              config: summarizeBrowserAccessConfig(config),
              restartRequired: true,
            }),
            { headers },
          );
        }
        case "disable": {
          const disabled = await disableBrowserAccess(root);
          return Response.json(
            accessManagementMutationResponseSchema.parse({
              disabled,
              restartRequired: disabled,
            }),
            { headers },
          );
        }
        case "apply-policy": {
          if (!current)
            return Response.json(
              managementErrorSchema.parse({
                error: { code: "provider_not_configured" },
              }),
              { status: 409, headers },
            );
          const config = await saveBrowserAccessConfig(root, {
            ...current,
            policy: input.policy,
          });
          return Response.json(
            accessManagementMutationResponseSchema.parse({
              policy: config.policy,
              restartRequired: true,
            }),
            { headers },
          );
        }
        case "clear-policy": {
          if (!current?.policy)
            return Response.json(
              accessManagementMutationResponseSchema.parse({
                cleared: false,
                restartRequired: false,
              }),
              { headers },
            );
          const { policy: _, ...config } = current;
          await saveBrowserAccessConfig(root, config);
          return Response.json(
            accessManagementMutationResponseSchema.parse({
              cleared: true,
              restartRequired: true,
            }),
            { headers },
          );
        }
        case "test-policy": {
          if (!current)
            return Response.json(
              managementErrorSchema.parse({
                error: { code: "provider_not_configured" },
              }),
              { status: 409, headers },
            );
          const decision = current.policy
            ? evaluateBrowserAccessPolicy(current.policy, input.identity)
            : { matchedRoles: [], permissions: current.permissions };
          return Response.json(
            accessManagementMutationResponseSchema.parse({
              policyConfigured: Boolean(current.policy),
              ...decision,
            }),
            { headers },
          );
        }
      }
    }

    const parsed = keyManagementRequestSchema.safeParse(value);
    if (!parsed.success)
      return Response.json(
        managementErrorSchema.parse({
          error: { code: "validation_failed" },
        }),
        { status: 400, headers },
      );
    if (!permits(principal, "keys:manage")) return denied(403);
    const config = configuration();
    const input = parsed.data;
    if (input.action !== "create") {
      const exists = loadApiKeys(database, config).some(
        (key) => key.id === input.keyId,
      );
      if (!exists)
        return Response.json(
          managementErrorSchema.parse({ error: { code: "key_not_found" } }),
          { status: 404, headers },
        );
    }
    switch (input.action) {
      case "create": {
        const { record, rawKey } = createApiKey(
          database,
          config,
          input.name,
          input.expiresDays,
          input.scopes,
        );
        return Response.json(
          keyManagementMutationResponseSchema.parse({
            key: publicApiKey(record),
            secret: rawKey,
          }),
          { status: 201, headers },
        );
      }
      case "rotate": {
        const { record, rawKey } = rotateApiKey(database, config, input.keyId);
        return Response.json(
          keyManagementMutationResponseSchema.parse({
            key: publicApiKey(record),
            secret: rawKey,
          }),
          { headers },
        );
      }
      case "revoke":
        return Response.json(
          keyManagementMutationResponseSchema.parse({
            key: publicApiKey(revokeApiKey(database, config, input.keyId)),
          }),
          { headers },
        );
      case "set-scopes":
        return Response.json(
          keyManagementMutationResponseSchema.parse({
            key: publicApiKey(
              setApiKeyScopes(database, config, input.keyId, input.scopes),
            ),
          }),
          { headers },
        );
    }
  };
}
