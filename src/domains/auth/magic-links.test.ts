import { expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession } from "../../db/client";
import { applyAccessControl, resolveAccessControl } from "./access-control";
import {
  consumeMagicLink,
  createMagicLinkService,
  issueMagicLink,
  magicLinkIssuer,
} from "./magic-links";
import { disableManagedUser, inviteManagedUser } from "./users";

async function withDatabase(
  run: (database: DatabaseSession, root: string) => void | Promise<void>,
): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), "localbase-magic-links-"));
  const database = new DatabaseSession();
  try {
    await run(database, root);
  } finally {
    database.close();
    await rm(root, { recursive: true, force: true });
  }
}

function configureUser(database: DatabaseSession, root: string): void {
  const db = database.get(root);
  applyAccessControl(db, {
    roles: [
      {
        name: "member",
        description: "Members",
        permissions: ["inference:chat"],
      },
      {
        name: "admin",
        description: "Administrators",
        permissions: ["access:manage"],
      },
    ],
    bindings: [{ kind: "email", role: "admin", email: "owner@example.com" }],
    defaultRole: "member",
  });
  inviteManagedUser(db, { email: "person@example.com", roles: ["member"] });
}

test("magic links are replacement, expiring, single-use credentials", async () => {
  await withDatabase((database, root) => {
    configureUser(database, root);
    const db = database.get(root);
    const now = Date.parse("2026-09-22T12:00:00.000Z");
    const issuer = magicLinkIssuer("https://localbase.example.com");
    const first = issueMagicLink(db, {
      email: "Person@Example.com",
      now,
    });
    const second = issueMagicLink(db, {
      email: "person@example.com",
      now: now + 1,
    });
    expect(first?.token).not.toBe(second?.token);
    expect(
      consumeMagicLink(db, { token: first?.token ?? "", issuer, now }),
    ).toBeNull();
    expect(
      consumeMagicLink(db, { token: second?.token ?? "", issuer, now }),
    ).toEqual({
      issuer,
      subject: expect.any(String),
      verifiedEmail: "person@example.com",
    });
    expect(
      consumeMagicLink(db, { token: second?.token ?? "", issuer, now }),
    ).toBeNull();

    const expired = issueMagicLink(db, { email: "person@example.com", now });
    expect(
      consumeMagicLink(db, {
        token: expired?.token ?? "",
        issuer,
        now: Date.parse(expired?.expiresAt ?? "") + 1,
      }),
    ).toBeNull();
  });
});

test("magic links disclose nothing for unknown or disabled users", async () => {
  await withDatabase((database, root) => {
    configureUser(database, root);
    const db = database.get(root);
    expect(issueMagicLink(db, { email: "unknown@example.com" })).toBeNull();
    const issued = issueMagicLink(db, { email: "person@example.com" });
    disableManagedUser(db, { email: "person@example.com" });
    expect(issueMagicLink(db, { email: "person@example.com" })).toBeNull();
    expect(
      consumeMagicLink(db, {
        token: issued?.token ?? "",
        issuer: "https://localbase.example.com/magic-link",
      }),
    ).toBeNull();
  });
});

test("one managed user can attach identities from multiple providers", async () => {
  await withDatabase((database, root) => {
    configureUser(database, root);
    const db = database.get(root);
    const email = "person@example.com";
    expect(
      resolveAccessControl(
        db,
        {
          issuer: "https://identity.example.com",
          subject: "oidc-subject",
          verifiedEmail: email,
        },
        { claimManagedUser: true },
      )?.permissions,
    ).toContain("inference:chat");
    expect(
      resolveAccessControl(
        db,
        {
          issuer: "https://github.com",
          subject: "12345",
          verifiedEmail: email,
        },
        { claimManagedUser: true },
      )?.permissions,
    ).toContain("inference:chat");
  });
});

test("magic-link delivery publishes a consumable URL and revokes failed mail", async () => {
  await withDatabase(async (database, root) => {
    configureUser(database, root);
    const db = database.get(root);
    const registration = {
      kind: "magic-link" as const,
      id: "email",
      name: "Email",
    };
    const delivered: string[] = [];
    const service = createMagicLinkService({
      db,
      origin: "https://localbase.example.com",
      emailDelivery: {
        host: "smtp.example.com",
        port: 587,
        security: "starttls",
        authentication: { kind: "none" },
        from: "localbase@example.com",
      },
      deliver: async (_config, email) => {
        delivered.push(email.text);
      },
    });
    await service.request(registration, "person@example.com");
    const link = delivered[0]?.match(/https:\/\/\S+/)?.[0];
    expect(link).toBeDefined();
    const token = new URL(link ?? "").hash.slice(1);
    expect(await service.consume(registration, token)).toMatchObject({
      issuer: "https://localbase.example.com/magic-link",
      verifiedEmail: "person@example.com",
    });

    let failure: unknown;
    let failedText = "";
    const failing = createMagicLinkService({
      db,
      origin: "https://localbase.example.com",
      emailDelivery: {
        host: "smtp.example.com",
        port: 587,
        security: "starttls",
        authentication: { kind: "none" },
        from: "localbase@example.com",
      },
      deliver: async (_config, email) => {
        failedText = email.text;
        throw new Error("provider response must not escape");
      },
      onDeliveryFailure: (error) => {
        failure = error;
      },
    });
    await failing.request(registration, "person@example.com");
    expect(failure).toBeInstanceOf(Error);
    const failedLink = failedText.match(/https:\/\/\S+/)?.[0] ?? "";
    const failedToken = new URL(failedLink).hash.slice(1);
    expect(await failing.consume(registration, failedToken)).toBeNull();
  });
});
