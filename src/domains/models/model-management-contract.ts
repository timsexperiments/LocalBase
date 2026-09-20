import { z } from "zod";

export const modelManagementActionSchema = z.enum([
  "install",
  "uninstall",
  "enable",
  "disable",
  "activate",
]);
export type ModelManagementAction = z.infer<typeof modelManagementActionSchema>;
export const modelManagementRequestSchema = z
  .object({
    modelId: z.string().min(1).max(200),
    action: modelManagementActionSchema,
  })
  .strict();
export type ModelManagementRequest = z.infer<
  typeof modelManagementRequestSchema
>;
const bytes = z.number().int().nonnegative();
export const modelManagementOperationSchema = z
  .object({
    action: modelManagementActionSchema,
    state: z.enum(["running", "complete", "failed"]),
    detail: z.string(),
    downloadedBytes: bytes.nullable(),
    totalBytes: bytes.nullable(),
  })
  .strict();
export type ModelManagementOperation = z.infer<
  typeof modelManagementOperationSchema
>;
export const modelManagementEntrySchema = z
  .object({
    id: z.string(),
    installed: z.boolean(),
    enabled: z.boolean(),
    active: z.boolean(),
    installedBytes: bytes,
    downloadBytes: bytes.nullable(),
    remainingDownloadBytes: bytes.nullable(),
    canInstall: z.boolean(),
    installUnavailableReason: z.string().nullable(),
    operation: modelManagementOperationSchema.nullable(),
  })
  .strict();
export type ModelManagementEntry = z.infer<typeof modelManagementEntrySchema>;
export const modelManagementSchema = z
  .object({
    storage: z
      .object({
        availableBytes: bytes.nullable(),
        totalBytes: bytes.nullable(),
      })
      .strict(),
    models: z.array(modelManagementEntrySchema),
  })
  .strict();
export type ModelManagement = z.infer<typeof modelManagementSchema>;
export type ModelManagementErrorCode =
  | "invalid_request"
  | "conflict"
  | "unsafe_path"
  | "insufficient_storage"
  | "storage_unavailable";
export class ModelManagementError extends Error {
  constructor(
    public readonly code: ModelManagementErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "ModelManagementError";
  }
}
