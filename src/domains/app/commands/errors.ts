import { z } from "zod";

export class CliInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CliInputError";
  }
}

export function formatZodError(
  error: z.ZodError,
  fieldName: (path: PropertyKey[]) => string = (path) =>
    path.join(".") || "input",
): string {
  return error.issues
    .map((issue) => `${fieldName(issue.path)}: ${issue.message}`)
    .join("; ");
}

export function toCliInputError(error: unknown): CliInputError | undefined {
  if (error instanceof CliInputError) return error;
  if (error instanceof z.ZodError)
    return new CliInputError(formatZodError(error));
}
