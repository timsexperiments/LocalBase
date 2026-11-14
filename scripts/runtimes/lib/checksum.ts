// The checksum helpers the runtime release scripts use, copied verbatim from the TypeScript
// gateway's src/utils/checksum.ts when the scripts stopped importing src/ (retirement R4b).
import { z } from "zod";

export const sha256Schema = z.string().regex(/^[a-fA-F0-9]{64}$/);
export const safeFilenameSchema = z
  .string()
  .min(1)
  .refine(
    (name) =>
      name === name.trim() &&
      name !== "." &&
      name !== ".." &&
      !name.includes("/") &&
      !name.includes("\\") &&
      !/[\u0000-\u001f\u007f]/.test(name),
    "must be a safe basename without path separators or control characters",
  );

function issueSummary(error: z.ZodError): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || "value"}: ${issue.message}`)
    .join("; ");
}

export function parseChecksumFile(content: string): Map<string, string> {
  const entries = new Map<string, string>();
  for (const [index, line] of content.split(/\r?\n/).entries()) {
    if (!line.trim()) continue;
    const match = /^([a-fA-F0-9]{64})\s+[ *]?(.+)$/.exec(line);
    if (!match) {
      throw new Error(`Invalid checksums.txt entry on line ${index + 1}.`);
    }
    const parsed = z
      .object({ digest: sha256Schema, filename: safeFilenameSchema })
      .strict()
      .safeParse({ digest: match[1], filename: match[2] });
    if (!parsed.success) {
      throw new Error(
        `Invalid checksums.txt entry on line ${index + 1}: ${issueSummary(parsed.error)}.`,
      );
    }
    if (entries.has(parsed.data.filename)) {
      throw new Error(
        `Invalid checksums.txt entry on line ${index + 1}: duplicate filename "${parsed.data.filename}".`,
      );
    }
    entries.set(parsed.data.filename, parsed.data.digest.toLowerCase());
  }
  return entries;
}
