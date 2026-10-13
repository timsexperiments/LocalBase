import { eq, lte, or } from "drizzle-orm";
import type { LocalBaseDatabase } from "../../db/client";
import { authBrowserSessionsTable } from "../../db/schema";
import type { BrowserIdentity } from "./browser-identity";

export const browserSessionIdleTtlMs = 7 * 24 * 60 * 60 * 1_000;
export const browserSessionAbsoluteTtlMs = 30 * 24 * 60 * 60 * 1_000;

export type StoredBrowserSession = Readonly<{
  ownerId: string;
  identity: BrowserIdentity;
  createdAt: number;
  lastSeenAt: number;
  idleExpiresAt: number;
  absoluteExpiresAt: number;
}>;

// Sessions are keyed by a SHA-256 hash of the cookie token, so the stored
// value cannot be replayed as a cookie.
export type BrowserSessionStore = Readonly<{
  create: (tokenHash: string, session: StoredBrowserSession) => void;
  find: (tokenHash: string) => StoredBrowserSession | null;
  touch: (tokenHash: string, lastSeenAt: number, idleExpiresAt: number) => void;
  delete: (tokenHash: string) => void;
  prune: (now: number) => void;
}>;

export function createMemoryBrowserSessionStore(
  maximum = 1_024,
): BrowserSessionStore {
  const sessions = new Map<string, StoredBrowserSession>();
  return {
    create(tokenHash, session) {
      while (sessions.size >= maximum) {
        const oldest = sessions.keys().next().value;
        if (typeof oldest !== "string") break;
        sessions.delete(oldest);
      }
      sessions.set(tokenHash, session);
    },
    find: (tokenHash) => sessions.get(tokenHash) ?? null,
    touch(tokenHash, lastSeenAt, idleExpiresAt) {
      const session = sessions.get(tokenHash);
      if (session)
        sessions.set(tokenHash, { ...session, lastSeenAt, idleExpiresAt });
    },
    delete: (tokenHash) => void sessions.delete(tokenHash),
    prune(now) {
      for (const [key, session] of sessions) {
        if (session.idleExpiresAt <= now || session.absoluteExpiresAt <= now)
          sessions.delete(key);
      }
    },
  };
}

export function createDatabaseBrowserSessionStore(
  db: LocalBaseDatabase,
): BrowserSessionStore {
  return {
    create(tokenHash, session) {
      db.insert(authBrowserSessionsTable)
        .values({
          tokenHash,
          ownerId: session.ownerId,
          issuer: session.identity.issuer,
          subject: session.identity.subject,
          verifiedEmail: session.identity.verifiedEmail ?? null,
          createdAt: session.createdAt,
          lastSeenAt: session.lastSeenAt,
          idleExpiresAt: session.idleExpiresAt,
          absoluteExpiresAt: session.absoluteExpiresAt,
        })
        .run();
    },
    find(tokenHash) {
      const row = db
        .select()
        .from(authBrowserSessionsTable)
        .where(eq(authBrowserSessionsTable.tokenHash, tokenHash))
        .get();
      if (!row) return null;
      return {
        ownerId: row.ownerId,
        identity: {
          issuer: row.issuer,
          subject: row.subject,
          ...(row.verifiedEmail ? { verifiedEmail: row.verifiedEmail } : {}),
        },
        createdAt: row.createdAt,
        lastSeenAt: row.lastSeenAt,
        idleExpiresAt: row.idleExpiresAt,
        absoluteExpiresAt: row.absoluteExpiresAt,
      };
    },
    touch(tokenHash, lastSeenAt, idleExpiresAt) {
      db.update(authBrowserSessionsTable)
        .set({ lastSeenAt, idleExpiresAt })
        .where(eq(authBrowserSessionsTable.tokenHash, tokenHash))
        .run();
    },
    delete(tokenHash) {
      db.delete(authBrowserSessionsTable)
        .where(eq(authBrowserSessionsTable.tokenHash, tokenHash))
        .run();
    },
    prune(now) {
      db.delete(authBrowserSessionsTable)
        .where(
          or(
            lte(authBrowserSessionsTable.idleExpiresAt, now),
            lte(authBrowserSessionsTable.absoluteExpiresAt, now),
          ),
        )
        .run();
    },
  };
}
