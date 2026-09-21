import { describe, expect, test } from "bun:test";
import { apiKeyStatus } from "./auth-management";

describe("API key status", () => {
  const now = Date.parse("2026-09-20T12:00:00.000Z");

  test("distinguishes active, expired, revoked, and malformed expiry values", () => {
    expect(apiKeyStatus({}, now)).toBe("Active");
    expect(apiKeyStatus({ expiresAt: "2026-09-21T12:00:00.000Z" }, now)).toBe(
      "Active",
    );
    expect(apiKeyStatus({ expiresAt: "2026-09-19T12:00:00.000Z" }, now)).toBe(
      "Expired",
    );
    expect(apiKeyStatus({ expiresAt: "invalid" }, now)).toBe("Expired");
    expect(
      apiKeyStatus(
        {
          expiresAt: "2026-09-21T12:00:00.000Z",
          revokedAt: "2026-09-18T12:00:00.000Z",
        },
        now,
      ),
    ).toBe("Revoked");
  });
});
