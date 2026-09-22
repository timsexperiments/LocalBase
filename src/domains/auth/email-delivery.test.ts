import { expect, test } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  disableEmailDelivery,
  EmailDeliveryError,
  emailDeliveryConfigPath,
  loadEmailDeliveryConfig,
  saveEmailDeliveryConfig,
  sendEmail,
  summarizeEmailDeliveryConfig,
  trySendEmail,
  type EmailDeliveryConfig,
} from "./email-delivery";

const config: EmailDeliveryConfig = {
  host: "smtp.example.com",
  port: 587,
  security: "starttls",
  authentication: {
    kind: "password",
    username: "localbase",
    password: "smtp-secret",
  },
  from: "localbase@example.com",
};

test("persists private SMTP configuration and returns a redacted summary", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-email-delivery-"));
  try {
    expect(await loadEmailDeliveryConfig(root)).toBeNull();
    expect(await saveEmailDeliveryConfig(root, config)).toEqual(config);
    expect(await loadEmailDeliveryConfig(root)).toEqual(config);
    expect((await stat(emailDeliveryConfigPath(root))).mode & 0o777).toBe(
      0o600,
    );
    expect(await readFile(emailDeliveryConfigPath(root), "utf8")).toContain(
      "smtp-secret",
    );
    expect(summarizeEmailDeliveryConfig(config)).toEqual({
      host: "smtp.example.com",
      port: 587,
      security: "starttls",
      authentication: "password",
      from: "localbase@example.com",
    });
    expect(JSON.stringify(summarizeEmailDeliveryConfig(config))).not.toContain(
      "smtp-secret",
    );
    expect(await disableEmailDelivery(root)).toBe(true);
    expect(await disableEmailDelivery(root)).toBe(false);
    expect(await loadEmailDeliveryConfig(root)).toBeNull();
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("reports safe SMTP failure categories without exposing credentials", async () => {
  const password = "smtp-secret-that-must-not-leak";
  try {
    await sendEmail(
      {
        ...config,
        host: "127.0.0.1",
        port: 1,
        authentication: {
          kind: "password",
          username: "localbase",
          password,
        },
      },
      {
        to: "recipient@example.com",
        subject: "Test",
        text: "Test",
      },
    );
    throw new Error("Expected email delivery to fail.");
  } catch (error) {
    expect(error).toBeInstanceOf(EmailDeliveryError);
    expect((error as EmailDeliveryError).category).toBe("connect");
    expect((error as Error).message).not.toContain(password);
  }
});

test("reports recoverable delivery failure without hiding caller state", async () => {
  expect(
    await trySendEmail(
      { ...config, host: "127.0.0.1", port: 1 },
      {
        to: "recipient@example.com",
        subject: "Invitation",
        text: "Sign in at https://localbase.example.com/app/login",
      },
    ),
  ).toBe(false);
});

test("rejects malformed configuration without exposing file contents", async () => {
  const root = await mkdtemp(join(tmpdir(), "localbase-email-delivery-"));
  try {
    await Bun.write(emailDeliveryConfigPath(root), '{"password":"leak-me"}');
    await expect(loadEmailDeliveryConfig(root)).rejects.toThrow(
      "Invalid email-delivery.json",
    );
    await expect(loadEmailDeliveryConfig(root)).rejects.not.toThrow("leak-me");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
