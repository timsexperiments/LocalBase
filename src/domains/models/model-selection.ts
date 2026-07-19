import { byId, type ModelKind } from "../../catalog";

export function validateModelList(
  ids: string[] | undefined,
  kind: ModelKind,
): string[] | undefined {
  if (!ids) return undefined;
  const invalid = ids.filter((id) => byId(id)?.kind !== kind);
  if (invalid.length > 0) {
    throw new Error(`Invalid ${kind} model ids: ${invalid.join(", ")}`);
  }
  return ids;
}
