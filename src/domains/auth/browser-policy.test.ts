import { expect, test } from "bun:test";
import {
  browserAccessPolicySchema,
  evaluateBrowserAccessPolicy,
} from "./browser-policy";

const policy = browserAccessPolicySchema.parse({
  roles: {
    admin: ["access:manage", "models:manage"],
    creator: ["inference:chat", "models:manage"],
  },
  bindings: [
    {
      role: "admin",
      match: {
        kind: "subject",
        issuer: "https://identity.example.com",
        subject: " exact subject ",
      },
    },
    {
      role: "creator",
      match: { kind: "email", email: "person@example.com" },
    },
    {
      role: "creator",
      match: { kind: "email-domain", domain: "EXAMPLE.COM" },
    },
  ],
});

test("unions matching roles and permissions deterministically", () => {
  expect(
    evaluateBrowserAccessPolicy(policy, {
      issuer: "https://identity.example.com",
      subject: " exact subject ",
      verifiedEmail: "person@example.com",
    }),
  ).toEqual({
    matchedRoles: ["admin", "creator"],
    permissions: ["inference:chat", "models:manage", "access:manage"],
  });
});

test("preserves exact subject matching and denies unmatched identities", () => {
  expect(
    evaluateBrowserAccessPolicy(policy, {
      issuer: "https://identity.example.com",
      subject: "exact subject",
    }),
  ).toEqual({ matchedRoles: [], permissions: [] });
});

test("rejects unknown roles and policies without an access administrator", () => {
  expect(
    browserAccessPolicySchema.safeParse({
      roles: { user: ["inference:chat"] },
      bindings: [
        { role: "missing", match: { kind: "email", email: "a@example.com" } },
      ],
    }).success,
  ).toBe(false);
  expect(
    browserAccessPolicySchema.safeParse({
      roles: { user: ["inference:chat"] },
      bindings: [
        { role: "user", match: { kind: "email", email: "a@example.com" } },
      ],
    }).success,
  ).toBe(false);
});
