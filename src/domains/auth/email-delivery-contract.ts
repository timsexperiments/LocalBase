import { z } from "zod";

const smtpHostSchema = z
  .string()
  .min(1)
  .max(253)
  .refine((value) => value.trim() === value && !/\s/.test(value));

export const emailDeliveryConfigSchema = z
  .object({
    host: smtpHostSchema,
    port: z.number().int().min(1).max(65_535),
    security: z.enum(["tls", "starttls"]),
    authentication: z.discriminatedUnion("kind", [
      z.object({ kind: z.literal("none") }).strict(),
      z
        .object({
          kind: z.literal("password"),
          username: z.string().min(1).max(512),
          password: z.string().min(1).max(4_096),
        })
        .strict(),
    ]),
    from: z.email().max(320),
  })
  .strict();

export type EmailDeliveryConfig = z.infer<typeof emailDeliveryConfigSchema>;

export const emailDeliveryConfigSummarySchema = emailDeliveryConfigSchema
  .omit({ authentication: true })
  .extend({ authentication: z.enum(["none", "password"]) })
  .strict();

export type EmailDeliveryConfigSummary = z.infer<
  typeof emailDeliveryConfigSummarySchema
>;

export const outboundEmailSchema = z
  .object({
    to: z.email().max(320),
    subject: z.string().min(1).max(200),
    text: z.string().min(1).max(100_000),
    html: z.string().min(1).max(200_000).optional(),
  })
  .strict();

export type OutboundEmail = z.infer<typeof outboundEmailSchema>;

export const emailDeliveryFailureCategorySchema = z.enum([
  "dns",
  "connect",
  "tls",
  "authentication",
  "timeout",
  "recipient",
  "unknown",
]);

export type EmailDeliveryFailureCategory = z.infer<
  typeof emailDeliveryFailureCategorySchema
>;
