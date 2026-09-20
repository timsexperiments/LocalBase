import { afterEach, beforeEach, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSession, databasePath } from "../../db/client";
import {
  createApiKey,
  defaultConfig,
  loadApiKeys,
  resolveApiKey,
  revokeApiKey,
  rotateApiKey,
  setApiKeyScopes,
  type ApiKeyRecord,
} from "../../manager";
import {
  authorize,
  defaultApiKeyScopes,
  permissionSchema,
  principalOwnerId,
  principalSchema,
} from "./authorization";

let root: string;
let database: DatabaseSession;
beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "local-base-key-scopes-"));
  database = new DatabaseSession();
});
afterEach(() => {
  database.close();
  rmSync(root, { recursive: true, force: true });
});

function principal(record: ApiKeyRecord) {
  return principalSchema.options[1].parse({
    kind: "api-key",
    id: record.id,
    name: record.name,
    permissions: record.scopes,
  });
}

test("default keys grant only inference and model reads; explicit scopes support all permissions", () => {
  const config = defaultConfig(root);
  const { record } = createApiKey(database, config, "default");
  expect(record.scopes).toEqual([
    "inference:chat",
    "inference:embeddings",
    "inference:image",
    "inference:video",
    "inference:speech",
    "inference:transcription",
    "models:read",
  ]);
  for (const permission of permissionSchema.options) {
    expect(
      authorize({
        principal: principal(record),
        requirement: { kind: "permission", permission },
      }).kind,
    ).toBe(
      defaultApiKeyScopes.includes(permission) ? "authorized" : "forbidden",
    );
  }
  const full = createApiKey(
    database,
    config,
    "explicit",
    undefined,
    permissionSchema.options,
  );
  database.closeRoot(root);
  expect(resolveApiKey(database, config, full.rawKey)?.scopes).toEqual(
    permissionSchema.options,
  );
});

test("scope changes, rotation, and revocation preserve identity and persist across sessions", () => {
  const config = defaultConfig(root);
  const original = createApiKey(database, config, "video", 30, [
    "models:read",
    "inference:video",
    "models:read",
  ]);
  expect(original.record.scopes).toEqual(["inference:video", "models:read"]);
  const owner = principalOwnerId(principal(original.record));
  const changed = setApiKeyScopes(database, config, original.record.id, [
    "inference:video",
  ]);
  expect(changed).toEqual({ ...original.record, scopes: ["inference:video"] });
  database.closeRoot(root);
  expect(resolveApiKey(database, config, original.rawKey)).toEqual(changed);

  const rotated = rotateApiKey(database, config, original.record.id);
  expect(rotated.rawKey).not.toBe(original.rawKey);
  expect(rotated.record).toMatchObject({
    id: changed.id,
    name: changed.name,
    createdAt: changed.createdAt,
    expiresAt: changed.expiresAt,
    scopes: changed.scopes,
  });
  expect(resolveApiKey(database, config, original.rawKey)).toBeUndefined();
  expect(resolveApiKey(database, config, rotated.rawKey)).toEqual(
    rotated.record,
  );
  expect(principalOwnerId(principal(rotated.record))).toBe(owner);

  const revoked = revokeApiKey(database, config, changed.id);
  expect(revoked).toEqual({ ...rotated.record, revokedAt: expect.any(String) });
  expect(resolveApiKey(database, config, rotated.rawKey)).toBeUndefined();
  const cleared = setApiKeyScopes(database, config, changed.id, []);
  expect(cleared).toEqual({ ...revoked, scopes: [] });
  const rotatedRevoked = rotateApiKey(database, config, changed.id);
  expect(rotatedRevoked.record.revokedAt).toBe(revoked.revokedAt);
  expect(rotatedRevoked.record.scopes).toEqual([]);
  expect(principalOwnerId(principal(rotatedRevoked.record))).toBe(owner);
  database.closeRoot(root);
  expect(loadApiKeys(database, config)).toEqual([rotatedRevoked.record]);
  expect(
    resolveApiKey(database, config, rotatedRevoked.rawKey),
  ).toBeUndefined();
});

test.each(["not-json", '"models:read"', '["*"]', '["models:write"]', "[null]"])(
  "fails closed on malformed stored scopes: %s",
  (scopes) => {
    const config = defaultConfig(root);
    const key = createApiKey(database, config, "malformed");
    const sqlite = new Database(databasePath(root));
    try {
      sqlite
        .prepare("UPDATE api_keys SET scopes = ? WHERE id = ?")
        .run(scopes, key.record.id);
      expect(() => resolveApiKey(database, config, key.rawKey)).toThrow(
        "Invalid API key configuration",
      );
    } finally {
      sqlite.close();
    }
  },
);
