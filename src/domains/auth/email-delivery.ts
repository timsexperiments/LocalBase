import { chmod, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import nodemailer from "nodemailer";
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

export class EmailDeliveryError extends Error {
  constructor(readonly category: EmailDeliveryFailureCategory) {
    super(`Email delivery failed (${category}).`);
    this.name = "EmailDeliveryError";
  }
}

export function emailDeliveryConfigPath(root: string): string {
  return join(root, "email-delivery.json");
}

export function summarizeEmailDeliveryConfig(
  config: EmailDeliveryConfig,
): EmailDeliveryConfigSummary {
  return emailDeliveryConfigSummarySchema.parse({
    host: config.host,
    port: config.port,
    security: config.security,
    authentication: config.authentication.kind,
    from: config.from,
  });
}

export async function loadEmailDeliveryConfig(
  root: string,
): Promise<EmailDeliveryConfig | null> {
  let contents: string;
  try {
    contents = await readFile(emailDeliveryConfigPath(root), "utf8");
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return null;
    throw new Error("Unable to read email delivery configuration.");
  }
  try {
    return emailDeliveryConfigSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid email-delivery.json. Configure email delivery again with the LocalBase CLI.",
    );
  }
}

export async function saveEmailDeliveryConfig(
  root: string,
  input: EmailDeliveryConfig,
): Promise<EmailDeliveryConfig> {
  const config = emailDeliveryConfigSchema.parse(input);
  const path = emailDeliveryConfigPath(root);
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await writeFile(temporary, `${JSON.stringify(config, null, 2)}\n`, {
      encoding: "utf8",
      flag: "wx",
      mode: 0o600,
    });
    await chmod(temporary, 0o600);
    await rename(temporary, path);
  } finally {
    await unlink(temporary).catch((error: unknown) => {
      if (!(
        error instanceof Error &&
        "code" in error &&
        error.code === "ENOENT"
      ))
        throw error;
    });
  }
  return config;
}

export async function disableEmailDelivery(root: string): Promise<boolean> {
  try {
    await unlink(emailDeliveryConfigPath(root));
    return true;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return false;
    throw new Error("Unable to disable email delivery.");
  }
}

function transporter(config: EmailDeliveryConfig) {
  return nodemailer.createTransport({
    host: config.host,
    port: config.port,
    secure: config.security === "tls",
    requireTLS: config.security === "starttls",
    dnsTimeout: 10_000,
    connectionTimeout: 10_000,
    greetingTimeout: 10_000,
    socketTimeout: 30_000,
    tls: { minVersion: "TLSv1.2" },
    ...(config.authentication.kind === "password"
      ? {
          auth: {
            user: config.authentication.username,
            pass: config.authentication.password,
          },
        }
      : {}),
  });
}

function deliveryFailure(error: unknown): EmailDeliveryError {
  const code =
    error instanceof Error && "code" in error && typeof error.code === "string"
      ? error.code
      : "";
  if (code === "EDNS") return new EmailDeliveryError("dns");
  if (code === "ETIMEDOUT") return new EmailDeliveryError("timeout");
  if (code === "EAUTH") return new EmailDeliveryError("authentication");
  if (code === "ETLS" || code === "EREQUIRETLS")
    return new EmailDeliveryError("tls");
  if (code === "EENVELOPE") return new EmailDeliveryError("recipient");
  if (code === "ECONNECTION" || code === "ESOCKET")
    return new EmailDeliveryError("connect");
  return new EmailDeliveryError("unknown");
}

export async function sendEmail(
  config: EmailDeliveryConfig,
  input: OutboundEmail,
): Promise<void> {
  const parsedConfig = emailDeliveryConfigSchema.parse(config);
  const email = outboundEmailSchema.parse(input);
  const client = transporter(parsedConfig);
  try {
    await client.sendMail({ from: parsedConfig.from, ...email });
  } catch (error) {
    throw deliveryFailure(error);
  } finally {
    client.close();
  }
}
