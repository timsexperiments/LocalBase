import {
  createApiKey,
  loadApiKeys,
  revokeApiKey,
  rotateApiKey,
} from "../../../../manager";
import type { AppContext } from "../../../../context";
import type { CommandExecution } from "../../../app/commands/framework";
import type {
  KeyIdInput,
  KeysCreateInput,
  KeysListInput,
} from "../../../app/commands/inputs";
import { publicApiKey } from "../../../app/commands/results";

export function runKeysList(
  _input: KeysListInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { keys: ReturnType<typeof publicApiKey>[] } } {
  const keys = loadApiKeys(ctx.database, ctx.config);
  if (keys.length === 0) {
    execution.output.info(
      "No API keys found. Create one with: local-base keys create --name default",
    );
    return { data: { keys: [] } };
  }
  for (const key of keys) {
    execution.output.info(
      `${key.id} | ${key.name} | prefix=${key.prefix} | created=${key.createdAt} | rotated=${key.lastRotatedAt}${key.expiresAt ? ` | expires=${key.expiresAt}` : ""}${key.revokedAt ? ` | revoked=${key.revokedAt}` : ""}`,
    );
  }
  return { data: { keys: keys.map(publicApiKey) } };
}

export function runKeysCreate(
  input: KeysCreateInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { key: ReturnType<typeof publicApiKey>; secret: string } } {
  const { record, rawKey } = createApiKey(
    ctx.database,
    ctx.config,
    input.name,
    input.expiresDays,
  );
  execution.output.info(
    `Created key id=${record.id} name=${record.name} prefix=${record.prefix}`,
  );
  if (!execution.global.json) {
    execution.output.info(`secret=${rawKey}`);
    execution.output.info("Store this secret now. It is not shown again.");
  }
  return { data: { key: publicApiKey(record), secret: rawKey } };
}

export function runKeysRevoke(
  input: KeyIdInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { key: ReturnType<typeof publicApiKey> } } {
  const record = revokeApiKey(ctx.database, ctx.config, input.keyId);
  execution.output.info(`Revoked key ${record.id} (${record.name})`);
  return { data: { key: publicApiKey(record) } };
}

export function runKeysRotate(
  input: KeyIdInput,
  ctx: AppContext,
  execution: CommandExecution,
): { data: { key: ReturnType<typeof publicApiKey>; secret: string } } {
  const { record, rawKey } = rotateApiKey(
    ctx.database,
    ctx.config,
    input.keyId,
  );
  execution.output.info(`Rotated key ${record.id} (${record.name})`);
  if (!execution.global.json) {
    execution.output.info(`new_secret=${rawKey}`);
    execution.output.info("Store this secret now. It is not shown again.");
  }
  return { data: { key: publicApiKey(record), secret: rawKey } };
}
