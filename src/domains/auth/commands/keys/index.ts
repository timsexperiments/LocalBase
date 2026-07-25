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

export function runKeysList(
  _input: KeysListInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  const keys = loadApiKeys(ctx.database, ctx.config);
  if (keys.length === 0) {
    execution.output.info(
      "No API keys found. Create one with: local-base keys create --name default",
    );
    return 0;
  }
  for (const key of keys) {
    execution.output.info(
      `${key.id} | ${key.name} | prefix=${key.prefix} | created=${key.createdAt} | rotated=${key.lastRotatedAt}${key.expiresAt ? ` | expires=${key.expiresAt}` : ""}${key.revokedAt ? ` | revoked=${key.revokedAt}` : ""}`,
    );
  }
  return 0;
}

export function runKeysCreate(
  input: KeysCreateInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  const { record, rawKey } = createApiKey(
    ctx.database,
    ctx.config,
    input.name,
    input.expiresDays,
  );
  execution.output.info(
    `Created key id=${record.id} name=${record.name} prefix=${record.prefix}`,
  );
  execution.output.info(`secret=${rawKey}`);
  execution.output.info("Store this secret now. It is not shown again.");
  return 0;
}

export function runKeysRevoke(
  input: KeyIdInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  const record = revokeApiKey(ctx.database, ctx.config, input.keyId);
  execution.output.info(`Revoked key ${record.id} (${record.name})`);
  return 0;
}

export function runKeysRotate(
  input: KeyIdInput,
  ctx: AppContext,
  execution: CommandExecution,
): number {
  const { record, rawKey } = rotateApiKey(
    ctx.database,
    ctx.config,
    input.keyId,
  );
  execution.output.info(`Rotated key ${record.id} (${record.name})`);
  execution.output.info(`new_secret=${rawKey}`);
  execution.output.info("Store this secret now. It is not shown again.");
  return 0;
}
