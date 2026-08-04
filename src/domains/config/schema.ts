import { z } from "zod";

export const hostSchema = z
  .string()
  .min(1)
  .max(253)
  .refine(
    (value) => value === value.trim() && !/\s/.test(value),
    "must not contain whitespace",
  );

export const portSchema = z.number().int().min(1).max(65_535);
