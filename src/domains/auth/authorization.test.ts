import { expect, test } from "bun:test";
import {
  authorize,
  permissionSchema,
  principalOwnerId,
  principalSchema,
  type Principal,
  type Permission,
} from "./authorization";

const anonymous: Principal = { kind: "anonymous" };
const reader = principalSchema.parse({
  kind: "api-key",
  id: "key_reader",
  name: "Reader",
  permissions: ["models:read", "inference:video"],
});
const environment = principalSchema.parse({
  kind: "environment",
  permissions: [],
});

test("public routes allow every principal without granting protected access", () => {
  for (const principal of [anonymous, reader, environment]) {
    expect(authorize({ principal, requirement: { kind: "public" } })).toEqual({
      kind: "public",
    });
  }
  expect(
    authorize({ principal: anonymous, requirement: { kind: "authenticated" } }),
  ).toEqual({ kind: "unauthenticated" });
  for (const permission of permissionSchema.options) {
    expect(
      authorize({
        principal: anonymous,
        requirement: { kind: "permission", permission },
      }),
    ).toEqual({ kind: "unauthenticated" });
  }
});

test("authentication alone does not grant permissions", () => {
  const cases: {
    principal: Principal;
    granted: Permission[];
    denied: Permission[];
  }[] = [
    {
      principal: reader,
      granted: ["models:read", "inference:video"],
      denied: [
        "inference:chat",
        "inference:embeddings",
        "inference:image",
        "inference:speech",
        "inference:transcription",
        "models:manage",
        "configuration:read",
        "configuration:manage",
        "keys:read",
        "keys:manage",
        "access:read",
        "access:manage",
        "sessions:read",
        "sessions:revoke",
        "system:read",
        "system:manage",
      ],
    },
    { principal: environment, granted: [], denied: permissionSchema.options },
  ];
  for (const { principal, granted, denied } of cases) {
    if (principal.kind === "anonymous")
      throw new Error("Expected a credential.");
    expect(
      authorize({ principal, requirement: { kind: "authenticated" } }),
    ).toEqual({ kind: "authorized", principal });
    for (const permission of granted) {
      expect(
        authorize({
          principal,
          requirement: { kind: "permission", permission },
        }),
      ).toEqual({ kind: "authorized", principal });
    }
    for (const permission of denied) {
      expect(
        authorize({
          principal,
          requirement: { kind: "permission", permission },
        }),
      ).toEqual({ kind: "forbidden", permission });
    }
  }
});

test("current unrestricted credentials can perform every gateway operation", () => {
  for (const identity of [
    { kind: "api-key", id: "key_full", name: "Full access" },
    { kind: "environment" },
  ]) {
    const principal = principalSchema.parse({
      ...identity,
      permissions: permissionSchema.options,
    });
    for (const permission of permissionSchema.options) {
      expect(
        authorize({
          principal,
          requirement: { kind: "permission", permission },
        }).kind,
      ).toBe("authorized");
    }
  }
});

test.each([
  null,
  { kind: "unknown" },
  { kind: "anonymous", permissions: ["inference:chat"] },
  { kind: "environment" },
  { kind: "environment", permissions: ["*"] },
  { kind: "environment", permissions: ["models:write"] },
  { kind: "api-key", id: "", name: "Reader", permissions: [] },
  { kind: "api-key", id: "key_reader", permissions: [] },
  {
    kind: "api-key",
    id: "key_reader",
    name: "Reader",
    permissions: "models:read",
  },
  { kind: "environment", permissions: [], token: "secret" },
])("rejects malformed principal input: %j", (input) => {
  expect(principalSchema.safeParse(input).success).toBe(false);
});

test("parsed principals and grants are immutable", () => {
  expect(Object.isFrozen(reader)).toBe(true);
  if (reader.kind === "anonymous") throw new Error("Expected a key principal.");
  expect(Object.isFrozen(reader.permissions)).toBe(true);
});

test("video ownership stays stable and separates key and environment identities", () => {
  const key = principalSchema.parse({
    kind: "api-key",
    id: "environment",
    name: "Renamable display name",
    permissions: [],
  });
  if (key.kind !== "api-key" || environment.kind !== "environment") {
    throw new Error("Expected authenticated principals.");
  }
  expect(principalOwnerId(key)).toBe("api-key:environment");
  expect(principalOwnerId(environment)).toBe("environment");
  expect(
    principalOwnerId({
      ...key,
      name: "Renamed",
      permissions: ["inference:video"],
    }),
  ).toBe("api-key:environment");
});
