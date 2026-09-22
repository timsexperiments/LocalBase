import { describe, expect, test } from "bun:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  AuthManagement,
  assignedRolesGrantAccessManagement,
  apiKeyStatus,
  isCurrentManagedUser,
  reconcileInviteRoles,
  shouldSendInvitationEmail,
  starterAccessPolicy,
} from "./auth-management";

test("recognizes only the signed-in managed user", () => {
  expect(isCurrentManagedUser("Owner@Example.com", "owner@example.com")).toBe(
    true,
  );
  expect(isCurrentManagedUser("other@example.com", "owner@example.com")).toBe(
    false,
  );
  expect(isCurrentManagedUser("owner@example.com", undefined)).toBe(false);
});

test("requires a managed role to preserve current-user administration", () => {
  const roles = [
    { name: "admin", description: "", permissions: ["access:manage"] },
    { name: "member", description: "", permissions: ["inference:chat"] },
  ] as const;
  expect(assignedRolesGrantAccessManagement(["admin"], roles)).toBe(true);
  expect(assignedRolesGrantAccessManagement(["member"], roles)).toBe(false);
  expect(assignedRolesGrantAccessManagement([], roles)).toBe(false);
});

describe("API key status", () => {
  const now = Date.parse("2026-09-20T12:00:00.000Z");

  test("distinguishes active, expired, revoked, and malformed expiry values", () => {
    expect(apiKeyStatus({}, now)).toBe("Active");
    expect(apiKeyStatus({ expiresAt: "2026-09-21T12:00:00.000Z" }, now)).toBe(
      "Active",
    );
    expect(apiKeyStatus({ expiresAt: "2026-09-19T12:00:00.000Z" }, now)).toBe(
      "Expired",
    );
    expect(apiKeyStatus({ expiresAt: "invalid" }, now)).toBe("Expired");
    expect(
      apiKeyStatus(
        {
          expiresAt: "2026-09-21T12:00:00.000Z",
          revokedAt: "2026-09-18T12:00:00.000Z",
        },
        now,
      ),
    ).toBe("Revoked");
  });
});

test("starter policy grants chat by default without model management", () => {
  const policy = starterAccessPolicy("Owner@Example.com");
  expect(policy.defaultRole).toBe("member");
  expect(policy.bindings).toEqual([
    { kind: "email", role: "admin", email: "owner@example.com" },
  ]);
  expect(
    policy.roles.find(({ name }) => name === "member")?.permissions,
  ).toEqual(["inference:chat", "models:read"]);
  expect(
    policy.roles
      .find(({ name }) => name === "member")
      ?.permissions.includes("models:manage"),
  ).toBe(false);
  expect(
    policy.roles
      .find(({ name }) => name === "admin")
      ?.permissions.includes("access:manage"),
  ).toBe(true);
});

test("invite roles retain valid selections and replace deleted selections", () => {
  const roles = [
    { name: "admin", description: "", permissions: ["access:manage"] },
    { name: "member", description: "", permissions: ["inference:chat"] },
  ] as const;
  expect(reconcileInviteRoles(roles, ["admin"], "member")).toEqual(["admin"]);
  expect(reconcileInviteRoles(roles, ["deleted"], "member")).toEqual([
    "member",
  ]);
  expect(reconcileInviteRoles(roles, ["deleted"], null)).toEqual(["admin"]);
  expect(reconcileInviteRoles([], ["deleted"], null)).toEqual([]);
});

test("invitation email requires both a user choice and SMTP configuration", () => {
  expect(shouldSendInvitationEmail(true, true)).toBe(true);
  expect(shouldSendInvitationEmail(true, false)).toBe(false);
  expect(shouldSendInvitationEmail(false, true)).toBe(false);
});

test("does not offer starter policy creation before the policy read completes", () => {
  const markup = renderToStaticMarkup(
    createElement(AuthManagement, {
      connection: { kind: "session", verifiedEmail: "owner@example.com" },
    }),
  );
  expect(markup).toContain("Loading access policy");
  expect(markup).not.toContain("Create roles");
});
