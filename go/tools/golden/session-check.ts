// TS's verdict on every stored browser session of a root, for the shadow
// check (DESIGN §17): the session store and the rules of TS authenticate
// (src/ui/oidc-session.ts) applied to each row by its stored token hash,
// with no cookie involved. Only verdicts and SHA-256 fingerprints of the
// fields are printed, never the values (owner ids, emails, hashes).
//
//   bun run go/tools/golden/session-check.ts <root> <now-ms> > ts.json
import { createHash } from "node:crypto";
import { openReadOnlyDatabase } from "../../../src/db/client";
import { createDatabaseBrowserSessionStore } from "../../../src/domains/auth/browser-sessions";
import { authBrowserSessionsTable } from "../../../src/db/schema";
import { loadUiAccessConfig } from "../../../src/ui/access";

const [root, nowArg] = process.argv.slice(2);
const now = Number(nowArg);
const access = await loadUiAccessConfig(root!);
const registrations =
  access?.provider.kind === "direct" ? access.provider.registrations : [];
// As TS authenticate: sessions stay valid while their sign-in method is
// configured (no GitHub or magic-link registration on the audited root).
const issuers = new Set(
  registrations.flatMap((r) => (r.kind === "oidc" ? [r.issuer] : [])),
);
const touchEveryMs = 60 * 60 * 1_000;
const fp = (v: unknown) =>
  createHash("sha256").update(String(v ?? "")).digest("hex").slice(0, 16);

const { db, close } = openReadOnlyDatabase(root!);
const store = createDatabaseBrowserSessionStore(db);
const rows = db.select({ tokenHash: authBrowserSessionsTable.tokenHash }).from(authBrowserSessionsTable).all();
const out = rows.map(({ tokenHash }) => {
  const s = store.find(tokenHash);
  let verdict = "missing";
  if (s) {
    if (!issuers.has(s.identity.issuer)) verdict = "issuer";
    else if (s.idleExpiresAt <= now || s.absoluteExpiresAt <= now) verdict = "expired";
    else verdict = now - s.lastSeenAt >= touchEveryMs ? "valid-touch" : "valid";
  }
  return {
    row: fp(tokenHash),
    verdict,
    owner: fp(s?.ownerId),
    issuer: fp(s?.identity.issuer),
    subject: fp(s?.identity.subject),
    email: fp(s?.identity.verifiedEmail),
  };
});
close();
console.log(JSON.stringify(out));
