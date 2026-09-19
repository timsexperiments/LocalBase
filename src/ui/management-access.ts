import { join } from "node:path";
import { z } from "zod";

const managementAccessSchema = z
  .object({
    allowUiSessions: z.boolean(),
    apiKeyIds: z.array(
      z
        .string()
        .regex(
          /^key_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
        ),
    ),
  })
  .strict();

export async function loadManagementAccess(root: string) {
  let contents: string;
  try {
    contents = await Bun.file(join(root, "model-management.json")).text();
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return managementAccessSchema.parse({
        allowUiSessions: false,
        apiKeyIds: [],
      });
    throw new Error("Unable to read model-management.json.");
  }
  try {
    return managementAccessSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid model-management.json. Expected strict allowUiSessions and apiKeyIds configuration using stored key IDs, not credentials.",
    );
  }
}

/** The caller must resolve credentials through the gateway, never headers. */
export function canManageModels(
  credential: { ownerId: string } | undefined,
  config: Awaited<ReturnType<typeof loadManagementAccess>>,
): boolean {
  if (!credential) return false;
  if (/^ui-access:[0-9a-f]{64}$/.test(credential.ownerId))
    return config.allowUiSessions;
  return (
    credential.ownerId.startsWith("api-key:") &&
    config.apiKeyIds.includes(credential.ownerId.slice("api-key:".length))
  );
}
