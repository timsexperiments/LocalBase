import { Buffer } from "node:buffer";
import { eq } from "drizzle-orm";
import { z } from "zod";
import type { LocalBaseDatabase } from "../../db/client";
import {
  authMagicLinkTokensTable,
  authUserEmailsTable,
  authUsersTable,
} from "../../db/schema";
import type { BrowserIdentity } from "./browser-identity";
import type { MagicLinkAccessRegistration } from "./browser-access-contract";
import {
  sendEmail,
  type EmailDeliveryConfig,
  type OutboundEmail,
} from "./email-delivery";
import { managedUserEmailSchema } from "./users";

export const magicLinkTtlMs = 15 * 60 * 1_000;

export type MagicLinkSessionAdapter = Readonly<{
  request: (
    registration: MagicLinkAccessRegistration,
    email: string,
  ) => void | Promise<void>;
  consume: (
    registration: MagicLinkAccessRegistration,
    token: string,
  ) => BrowserIdentity | null | Promise<BrowserIdentity | null>;
}>;

const magicLinkTokenSchema = z
  .string()
  .length(43)
  .regex(/^[A-Za-z0-9_-]+$/);

function randomToken(): string {
  return Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString(
    "base64url",
  );
}

function tokenHash(token: string): string {
  return new Bun.CryptoHasher("sha256").update(token).digest("hex");
}

export function magicLinkIssuer(origin: string): string {
  return `${origin}/magic-link`;
}

export function issueMagicLink(
  db: LocalBaseDatabase,
  input: Readonly<{ email: string; now?: number }>,
): Readonly<{ email: string; token: string; expiresAt: string }> | null {
  const email = managedUserEmailSchema.parse(input.email);
  const now = input.now ?? Date.now();
  return db.transaction(
    () => {
      const user = db
        .select({
          id: authUsersTable.id,
          status: authUsersTable.status,
        })
        .from(authUserEmailsTable)
        .innerJoin(
          authUsersTable,
          eq(authUserEmailsTable.userId, authUsersTable.id),
        )
        .where(eq(authUserEmailsTable.email, email))
        .get();
      if (!user || !["pending", "active"].includes(user.status)) return null;
      const token = randomToken();
      const hash = tokenHash(token);
      const createdAt = new Date(now).toISOString();
      const expiresAt = new Date(now + magicLinkTtlMs).toISOString();
      db.insert(authMagicLinkTokensTable)
        .values({
          userId: user.id,
          tokenHash: hash,
          createdAt,
          expiresAt,
        })
        .onConflictDoUpdate({
          target: authMagicLinkTokensTable.userId,
          set: { tokenHash: hash, createdAt, expiresAt },
        })
        .run();
      return { email, token, expiresAt };
    },
    { behavior: "immediate" },
  );
}

export function revokeMagicLink(db: LocalBaseDatabase, token: string): void {
  const parsed = magicLinkTokenSchema.safeParse(token);
  if (!parsed.success) return;
  db.delete(authMagicLinkTokensTable)
    .where(eq(authMagicLinkTokensTable.tokenHash, tokenHash(parsed.data)))
    .run();
}

export function consumeMagicLink(
  db: LocalBaseDatabase,
  input: Readonly<{ token: string; issuer: string; now?: number }>,
): BrowserIdentity | null {
  const token = magicLinkTokenSchema.safeParse(input.token);
  if (!token.success) return null;
  const hash = tokenHash(token.data);
  const now = input.now ?? Date.now();
  return db.transaction(
    () => {
      const stored = db
        .select({
          userId: authMagicLinkTokensTable.userId,
          expiresAt: authMagicLinkTokensTable.expiresAt,
          status: authUsersTable.status,
          email: authUserEmailsTable.email,
        })
        .from(authMagicLinkTokensTable)
        .innerJoin(
          authUsersTable,
          eq(authMagicLinkTokensTable.userId, authUsersTable.id),
        )
        .innerJoin(
          authUserEmailsTable,
          eq(authMagicLinkTokensTable.userId, authUserEmailsTable.userId),
        )
        .where(eq(authMagicLinkTokensTable.tokenHash, hash))
        .get();
      db.delete(authMagicLinkTokensTable)
        .where(eq(authMagicLinkTokensTable.tokenHash, hash))
        .run();
      if (
        !stored ||
        !["pending", "active"].includes(stored.status) ||
        !Number.isFinite(Date.parse(stored.expiresAt)) ||
        Date.parse(stored.expiresAt) <= now
      )
        return null;
      return {
        issuer: input.issuer,
        subject: stored.userId,
        verifiedEmail: stored.email,
      };
    },
    { behavior: "immediate" },
  );
}

export function clearMagicLinks(db: LocalBaseDatabase): void {
  db.delete(authMagicLinkTokensTable).run();
}

export function createMagicLinkService({
  db,
  origin,
  emailDelivery,
  deliver = sendEmail,
  onDeliveryFailure,
}: Readonly<{
  db: LocalBaseDatabase;
  origin: string;
  emailDelivery: EmailDeliveryConfig;
  deliver?: (
    config: EmailDeliveryConfig,
    email: OutboundEmail,
  ) => Promise<void>;
  onDeliveryFailure?: (error: unknown) => void;
}>): MagicLinkSessionAdapter {
  const issuer = magicLinkIssuer(origin);
  return {
    async request(_registration, email) {
      const issued = issueMagicLink(db, { email });
      if (!issued) return;
      const signIn = new URL("/magic-link/callback", origin);
      signIn.searchParams.set("token", issued.token);
      try {
        await deliver(emailDelivery, {
          to: issued.email,
          subject: "Sign in to LocalBase",
          text: `Sign in to LocalBase:\n\n${signIn.href}\n\nThis link expires in 15 minutes and can be used once.`,
        });
      } catch (error) {
        revokeMagicLink(db, issued.token);
        onDeliveryFailure?.(error);
      }
    },
    consume: (_registration, token) => consumeMagicLink(db, { token, issuer }),
  };
}
