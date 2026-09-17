import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { loadGatewayListener } from "./gateway-listener";

let root: string;
beforeEach(async () => {
  root = await mkdtemp(join(tmpdir(), "localbase-listener-"));
});
afterEach(async () => {
  await rm(root, { recursive: true, force: true });
});

test("defaults to loopback when absent and applies per-field CLI overrides", async () => {
  expect(await loadGatewayListener(root)).toEqual({
    host: "127.0.0.1",
    port: 2273,
  });
  await Bun.write(
    join(root, "gateway-listener.json"),
    JSON.stringify({ host: "0.0.0.0", port: 9000 }),
  );
  expect(await loadGatewayListener(root)).toEqual({
    host: "0.0.0.0",
    port: 9000,
  });
  expect(await loadGatewayListener(root, { host: "::1" })).toEqual({
    host: "::1",
    port: 9000,
  });
  expect(await loadGatewayListener(root, { port: 8000 })).toEqual({
    host: "0.0.0.0",
    port: 8000,
  });
  await Bun.write(
    join(root, "gateway-listener.json"),
    JSON.stringify({ host: "localhost" }),
  );
  expect(await loadGatewayListener(root)).toEqual({
    host: "localhost",
    port: 2273,
  });
});

test.each([
  "{",
  "null",
  "[]",
  '{"host":""}',
  '{"host":" bad "}',
  '{"port":"2273"}',
  '{"port":0}',
  '{"port":65536}',
  '{"port":1.5}',
  '{"auth":false}',
])(
  "rejects malformed listener config %s even with CLI overrides",
  async (contents) => {
    await Bun.write(join(root, "gateway-listener.json"), contents);
    await expect(
      loadGatewayListener(root, { host: "127.0.0.1", port: 2273 }),
    ).rejects.toThrow("Invalid gateway-listener.json");
  },
);

test("does not treat an unreadable config as an absent config", async () => {
  await mkdir(join(root, "gateway-listener.json"));
  await expect(loadGatewayListener(root)).rejects.toThrow();
});
