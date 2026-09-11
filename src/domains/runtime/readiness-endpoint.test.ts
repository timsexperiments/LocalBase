import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  startGatewayFixture,
  type GatewayFixture,
} from "../../test/gateway-fixture";
import { gatewayReadinessSchema } from "./readiness";

describe("gateway readiness endpoint", () => {
  let gateway: GatewayFixture;

  beforeAll(async () => {
    gateway = await startGatewayFixture({ auth: { mode: "either" } });
  }, 30_000);

  afterAll(async () => {
    await gateway?.stop();
  }, 10_000);

  test("is public, strict, and does not launch a runtime", async () => {
    const before = await Promise.all([
      gateway.readLlmRuntimeLaunches(),
      gateway.readSttRuntimeLaunches(),
      gateway.readImageRuntimeLaunches(),
    ]);
    expect(before).toEqual([[], [], []]);

    const response = await fetch(`${gateway.baseUrl}/health/ready`);
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toBe(
      "application/json; charset=utf-8",
    );
    const body = gatewayReadinessSchema.parse(await response.json());
    expect(body).toEqual({
      status: "ready",
      reason: "request_admission_available",
      modalities: ["llm", "stt", "image"],
    });
    expect(response.headers.get("content-length")).toBe(
      String(new TextEncoder().encode(JSON.stringify(body)).byteLength),
    );

    const serialized = JSON.stringify(body);
    expect(serialized).not.toContain(gateway.root);
    expect(serialized).not.toContain("qwen2.5-coder-1.5b-instruct-q4_k_m");
    expect(serialized).not.toContain("lb_");
    await expect(
      Promise.all([
        gateway.readLlmRuntimeLaunches(),
        gateway.readSttRuntimeLaunches(),
        gateway.readImageRuntimeLaunches(),
      ]),
    ).resolves.toEqual([[], [], []]);
  });

  test("returns the same readiness status and headers for HEAD", async () => {
    const get = await fetch(`${gateway.baseUrl}/health/ready`);
    const head = await fetch(`${gateway.baseUrl}/health/ready`, {
      method: "HEAD",
    });

    expect(head.status).toBe(get.status);
    expect(head.headers.get("content-type")).toBe(
      get.headers.get("content-type"),
    );
    expect(head.headers.get("content-length")).toBe(
      get.headers.get("content-length"),
    );
    expect(await head.text()).toBe("");
  });

  test("rejects non-probe methods without authenticating or launching", async () => {
    const response = await fetch(`${gateway.baseUrl}/health/ready`, {
      method: "POST",
    });

    expect(response.status).toBe(405);
    expect(response.headers.get("allow")).toBe("GET, HEAD");
    await expect(response.json()).resolves.toMatchObject({
      error: { code: "method_not_allowed" },
    });
    await expect(gateway.readLlmRuntimeLaunches()).resolves.toEqual([]);
  });
});
