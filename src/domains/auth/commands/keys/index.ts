import { createApiKey, loadApiKeys, revokeApiKey, rotateApiKey, type LocalBaseConfig } from "../../../../manager";
import { parseFlag, toInt } from "../../../../utils/args";

export function runKeys(args: string[], config: LocalBaseConfig): number {
  const sub = args[1] ?? "list";

  if (sub === "list") {
    const keys = loadApiKeys(config);
    if (keys.length === 0) {
      console.log("No API keys found. Create one with: local-base keys create --name default");
      return 0;
    }
    for (const key of keys) {
      console.log(
        `${key.id} | ${key.name} | prefix=${key.prefix} | created=${key.createdAt} | rotated=${key.lastRotatedAt}${key.expiresAt ? ` | expires=${key.expiresAt}` : ""}${key.revokedAt ? ` | revoked=${key.revokedAt}` : ""}`
      );
    }
    return 0;
  }

  if (sub === "create") {
    const name = parseFlag(args, "--name") ?? "manual";
    const expiresDays = toInt(parseFlag(args, "--expires-days"), 0);
    const { record, rawKey } = createApiKey(config, name, expiresDays > 0 ? expiresDays : undefined);
    console.log(`Created key id=${record.id} name=${record.name} prefix=${record.prefix}`);
    console.log(`secret=${rawKey}`);
    console.log("Store this secret now. It is not shown again.");
    return 0;
  }

  if (sub === "revoke") {
    const id = args[2];
    if (!id) {
      console.error("keys revoke requires <key_id>");
      return 2;
    }
    const record = revokeApiKey(config, id);
    console.log(`Revoked key ${record.id} (${record.name})`);
    return 0;
  }

  if (sub === "rotate") {
    const id = args[2];
    if (!id) {
      console.error("keys rotate requires <key_id>");
      return 2;
    }
    const { record, rawKey } = rotateApiKey(config, id);
    console.log(`Rotated key ${record.id} (${record.name})`);
    console.log(`new_secret=${rawKey}`);
    console.log("Store this secret now. It is not shown again.");
    return 0;
  }

  console.error(`Unknown keys subcommand: ${sub}`);
  return 2;
}
