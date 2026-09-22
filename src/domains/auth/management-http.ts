import type { DatabaseSession } from "../../db/client";
import { withRootOperation } from "../service/ownership";
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
  removeAccessRegistration,
  saveBrowserAccessConfig,
  summarizeBrowserAccessConfig,
  upsertAccessRegistration,
} from "./browser-access";
import {
  applyAccessControl,
  clearAccessControl,
  loadAccessControl,
  resolveAccessControl,
} from "./access-control";
import { publicApiKey } from "./api-key-public";
import { BrowserAccessError } from "./errors";
import {
  disableManagedUser,
  enableManagedUser,
  inviteManagedUser,
  listManagedUsers,
  removeManagedUser,
  replaceManagedUserRoles,
} from "./users";
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

type ManagementErrorCode =
  | "invalid_api_key"
  | "insufficient_permissions"
  | "validation_failed"
  | "payload_too_large"
  | "request_aborted"
  | "provider_not_configured"
  | "policy_not_configured"
  | "registration_not_found"
  | "key_not_found"
  | "managed_user_not_found"
  | "managed_user_exists"
  | "role_not_found"
  | "policy_conflict";

const errorMessages: Record<ManagementErrorCode, string> = {
  invalid_api_key: "Authentication is required.",
  insufficient_permissions: "This identity cannot perform that action.",
  validation_failed: "The management request is invalid.",
  payload_too_large: "The management request exceeds the size limit.",
  request_aborted: "The management request was cancelled.",
  provider_not_configured: "Configure a browser identity provider first.",
  policy_not_configured: "Configure a browser access policy first.",
  registration_not_found: "The identity provider registration was not found.",
  key_not_found: "The API key was not found.",
  managed_user_not_found: "The managed user was not found.",
  managed_user_exists: "The managed user already exists.",
  role_not_found: "One or more browser access roles were not found.",
  policy_conflict: "The browser access policy cannot be changed.",
};

function errorBody(code: ManagementErrorCode) {
  return managementErrorSchema.parse({
    error: { code, message: errorMessages[code] },
  });
}

function denied(status: 401 | 403): Response {
  return Response.json(
    errorBody(status === 401 ? "invalid_api_key" : "insufficient_permissions"),
    { status, headers: { "cache-control": "no-store" } },
  );
}

function permits(principal: Principal, permission: Permission): boolean {
  return (
    authorize({ principal, requirement: { kind: "permission", permission } })
      .kind === "authorized"
  );
}

function domainErrorResponse(
  error: unknown,
  headers: HeadersInit,
): Response | null {
  if (!(error instanceof BrowserAccessError)) return null;
  const mapped = {
    "policy-not-configured": ["policy_not_configured", 409],
    "managed-user-not-found": ["managed_user_not_found", 404],
    "managed-user-exists": ["managed_user_exists", 409],
    "role-not-found": ["role_not_found", 400],
    "policy-conflict": ["policy_conflict", 409],
  } as const satisfies Record<
    typeof error.code,
    readonly [ManagementErrorCode, 400 | 404 | 409]
  >;
  const [code, status] = mapped[error.code];
  return Response.json(errorBody(code), { status, headers });
}

async function readChunk(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  signal: AbortSignal,
) {
  if (signal.aborted) throw new Error("aborted");
  let rejectAbort: (() => void) | undefined;
  const aborted = new Promise<never>((_, reject) => {
    rejectAbort = () => reject(new Error("aborted"));
    signal.addEventListener("abort", rejectAbort, { once: true });
  });
  try {
    return await Promise.race([reader.read(), aborted]);
  } finally {
    if (rejectAbort) signal.removeEventListener("abort", rejectAbort);
  }
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
      const { done, value } = await readChunk(reader, request.signal);
      if (done) break;
      size += value.byteLength;
      if (size > maximumBodyBytes) throw new Error("too_large");
      chunks.push(value);
    }
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
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
        const db = database.get(root);
        const policy = loadAccessControl(db);
        return Response.json(
          accessManagementReadResponseSchema.parse({
            config: config ? summarizeBrowserAccessConfig(config) : null,
            policy,
            users: listManagedUsers(db),
            roles: policy?.roles ?? [],
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

    if (
      pathname === accessPath &&
      !permits(principal, "access:read") &&
      !permits(principal, "access:manage")
    )
      return denied(403);
    if (pathname === keysPath && !permits(principal, "keys:manage"))
      return denied(403);

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
      return Response.json(errorBody(code), {
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
        return Response.json(errorBody("validation_failed"), {
          status: 400,
          headers,
        });
      const input = parsed.data;
      const required =
        input.action === "test-policy" ? "access:read" : "access:manage";
      if (!permits(principal, required)) return denied(403);
      if (input.action === "test-policy") {
        const current = await loadBrowserAccessConfig(root);
        if (!current)
          return Response.json(errorBody("provider_not_configured"), {
            status: 409,
            headers,
          });
        const policy = loadAccessControl(database.get(root));
        const decision = resolveAccessControl(
          database.get(root),
          input.identity,
        ) ?? {
          matchedRoles: [],
          permissions: current.permissions,
        };
        return Response.json(
          accessManagementMutationResponseSchema.parse({
            policyConfigured: policy !== null,
            ...decision,
          }),
          { headers },
        );
      }
      try {
        return await withRootOperation(
          root,
          "update browser access",
          async (canonicalRoot) => {
            const current = await loadBrowserAccessConfig(canonicalRoot);
            switch (input.action) {
              case "configure-cloudflare": {
                const config = await saveBrowserAccessConfig(canonicalRoot, {
                  provider: input.provider,
                  origin: input.origin,
                  permissions: input.permissions,
                });
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    config: summarizeBrowserAccessConfig(config),
                    restartRequired: true,
                  }),
                  { headers },
                );
              }
              case "upsert-oidc": {
                const config = await saveBrowserAccessConfig(
                  canonicalRoot,
                  upsertAccessRegistration(current, input),
                );
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    config: summarizeBrowserAccessConfig(config),
                    restartRequired: true,
                  }),
                  { headers },
                );
              }
              case "upsert-github": {
                const config = await saveBrowserAccessConfig(
                  canonicalRoot,
                  upsertAccessRegistration(current, input),
                );
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    config: summarizeBrowserAccessConfig(config),
                    restartRequired: true,
                  }),
                  { headers },
                );
              }
              case "remove-registration": {
                const removal = removeAccessRegistration(
                  current,
                  input.registrationId,
                );
                if (removal.kind === "not-found")
                  return Response.json(errorBody("registration_not_found"), {
                    status: 404,
                    headers,
                  });
                let config = null;
                if (removal.kind === "disabled") {
                  await disableBrowserAccess(canonicalRoot);
                } else {
                  config = summarizeBrowserAccessConfig(
                    await saveBrowserAccessConfig(
                      canonicalRoot,
                      removal.config,
                    ),
                  );
                }
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    removedRegistrationId: input.registrationId,
                    config,
                    restartRequired: true,
                  }),
                  { headers },
                );
              }
              case "disable": {
                const disabled = await disableBrowserAccess(canonicalRoot);
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    disabled,
                    restartRequired: disabled,
                  }),
                  { headers },
                );
              }
              case "invite-user": {
                if (!current)
                  return Response.json(errorBody("provider_not_configured"), {
                    status: 409,
                    headers,
                  });
                const user = inviteManagedUser(database.get(canonicalRoot), {
                  email: input.email,
                  roles: input.roles,
                });
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    user,
                    signInUrl: new URL("/app", current.origin).toString(),
                  }),
                  { status: 201, headers },
                );
              }
              case "replace-user-roles": {
                const user = replaceManagedUserRoles(
                  database.get(canonicalRoot),
                  {
                    email: input.email,
                    roles: input.roles,
                  },
                );
                return Response.json(
                  accessManagementMutationResponseSchema.parse({ user }),
                  { headers },
                );
              }
              case "enable-user": {
                const user = enableManagedUser(database.get(canonicalRoot), {
                  email: input.email,
                });
                return Response.json(
                  accessManagementMutationResponseSchema.parse({ user }),
                  { headers },
                );
              }
              case "disable-user": {
                const user = disableManagedUser(database.get(canonicalRoot), {
                  email: input.email,
                });
                return Response.json(
                  accessManagementMutationResponseSchema.parse({ user }),
                  { headers },
                );
              }
              case "remove-user": {
                const user = removeManagedUser(database.get(canonicalRoot), {
                  email: input.email,
                });
                return Response.json(
                  accessManagementMutationResponseSchema.parse({ user }),
                  { headers },
                );
              }
              case "apply-policy": {
                if (!current)
                  return Response.json(errorBody("provider_not_configured"), {
                    status: 409,
                    headers,
                  });
                const policy = applyAccessControl(
                  database.get(canonicalRoot),
                  input.policy,
                );
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    policy,
                    restartRequired: false,
                  }),
                  { headers },
                );
              }
              case "clear-policy": {
                const cleared = clearAccessControl(database.get(canonicalRoot));
                return Response.json(
                  accessManagementMutationResponseSchema.parse({
                    cleared,
                    restartRequired: false,
                  }),
                  { headers },
                );
              }
            }
          },
        );
      } catch (error) {
        const response = domainErrorResponse(error, headers);
        if (response) return response;
        throw error;
      }
    }

    const parsed = keyManagementRequestSchema.safeParse(value);
    if (!parsed.success)
      return Response.json(errorBody("validation_failed"), {
        status: 400,
        headers,
      });
    const config = configuration();
    const input = parsed.data;
    if (input.action !== "create") {
      const exists = loadApiKeys(database, config).some(
        (key) => key.id === input.keyId,
      );
      if (!exists)
        return Response.json(errorBody("key_not_found"), {
          status: 404,
          headers,
        });
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
