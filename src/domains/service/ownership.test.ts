import { expect, test } from "bun:test";
import {
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  acquireGatewayLease,
  acquireServeInitializationLease,
  canonicalRoot,
  canonicalRootHash,
  getGatewayInstanceState,
  withServiceStartHandoff,
  withRootOperation,
} from "./ownership";
import { gatewayHealthSchema, gatewayIdentitySchema } from "../runtime/health";
import { LOCALBASE_VERSION } from "../../version";

function reservePort(): number {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const random = new Uint32Array(1);
    crypto.getRandomValues(random);
    const port = 20_000 + (random[0] % 40_000);
    try {
      const reservation = Bun.serve({
        hostname: "127.0.0.1",
        port,
        fetch: () => new Response("reserved"),
      });
      reservation.stop(true);
      return port;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EADDRINUSE") throw error;
    }
  }
  throw new Error("Could not reserve a gateway ownership test port.");
}

test("uses one authenticated gateway lease for canonical root aliases", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-root-lease-"));
  const target = join(directory, "target");
  const alias = join(directory, "alias");
  mkdirSync(target);
  symlinkSync(target, alias);
  let lease: Awaited<ReturnType<typeof acquireGatewayLease>> | undefined;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: reservePort(),
    fetch: (request) => {
      const pathname = new URL(request.url).pathname;
      if (pathname === "/_localbase/instance" && lease) {
        if (
          request.headers.get("x-localbase-instance-token") !==
          lease.instance.instanceToken
        ) {
          return new Response(null, { status: 404 });
        }
        return Response.json(
          gatewayIdentitySchema.parse({
            instanceId: lease.instance.instanceId,
            rootHash: lease.instance.rootHash,
          }),
        );
      }
      if (pathname === "/health" && lease) {
        return Response.json(
          gatewayHealthSchema.parse({
            status: "ok",
            version: LOCALBASE_VERSION,
            uptimeSeconds: 0,
            modalities: {
              llm: { configured: true, state: "idle" },
              stt: { configured: false, state: "disabled" },
              image: { configured: false, state: "disabled" },
            },
          }),
        );
      }
      return new Response(null, { status: 404 });
    },
  });

  try {
    expect(await canonicalRoot(alias)).toBe(realpathSync(target));
    lease = await acquireGatewayLease(alias, {
      host: "127.0.0.1",
      port: server.port!,
    });
    await expect(
      acquireGatewayLease(target, { host: "127.0.0.1", port: server.port! }),
    ).rejects.toThrow("already owns");
    expect(await getGatewayInstanceState(target)).toMatchObject({
      state: "active",
      instance: { instanceId: lease.instance.instanceId },
    });
    await lease.release();
    expect(await getGatewayInstanceState(target)).toMatchObject({
      state: "missing",
    });
  } finally {
    server.stop(true);
    await lease?.release();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("serializes operations outside the managed root", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-operation-lock-"));
  const root = join(directory, "managed-root");
  const coordinationDirectory = join(directory, "coordination");
  mkdirSync(root);
  let releaseFirst: (() => void) | undefined;
  const enteredFirst = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  const order: string[] = [];

  try {
    const first = withRootOperation(
      root,
      "first",
      async () => {
        order.push("first");
        rmSync(root, { recursive: true, force: true });
        await enteredFirst;
      },
      { coordinationDirectory },
    );
    while (order.length === 0) await Bun.sleep(5);
    const second = withRootOperation(
      root,
      "second",
      async () => {
        order.push("second");
      },
      { coordinationDirectory },
    );
    await Bun.sleep(25);
    expect(order).toEqual(["first"]);
    releaseFirst?.();
    await Promise.all([first, second]);
    expect(order).toEqual(["first", "second"]);
    expect(
      await Bun.file(
        join(
          coordinationDirectory,
          `${new Bun.CryptoHasher("sha256").update(root).digest("hex")}.operation.lock`,
        ),
      ).exists(),
    ).toBe(false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("hands a managed start to its token-bound gateway before reset can proceed", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-start-handoff-"));
  const root = join(directory, "managed-root");
  const coordinationDirectory = join(directory, "coordination");
  const token = crypto.randomUUID();
  let resetEntered = false;

  try {
    await withServiceStartHandoff(
      root,
      async (_canonical, handoff) => {
        await handoff(token);
      },
      { coordinationDirectory },
    );
    const canonical = await canonicalRoot(root);
    const mutationLock = join(
      coordinationDirectory,
      `${canonicalRootHash(canonical)}.operation.lock.mutation`,
    );
    mkdirSync(mutationLock);
    writeFileSync(
      join(mutationLock, "owner.json"),
      JSON.stringify({
        version: 1,
        token: crypto.randomUUID(),
        pid: process.pid,
      }),
    );
    const reset = withRootOperation(
      root,
      "reset",
      async () => {
        resetEntered = true;
      },
      { coordinationDirectory },
    );
    const claiming = acquireServeInitializationLease(canonical, token, {
      coordinationDirectory,
    });
    await Bun.sleep(25);
    expect(resetEntered).toBe(false);

    rmSync(mutationLock, { recursive: true });
    const initialization = await claiming;
    await Bun.sleep(25);
    expect(resetEntered).toBe(false);
    const gateway = await acquireGatewayLease(canonical, {
      host: "127.0.0.1",
      port: 23_731,
      serviceId: `com.localbase.gateway.${canonicalRootHash(canonical)}`,
      serviceToken: token,
    });
    await initialization.release();
    await gateway.release();
    await reset;
    expect(resetEntered).toBe(true);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("serializes abandoned cleanup before a contender can claim ownership", async () => {
  const directory = mkdtempSync(join(tmpdir(), "local-base-operation-race-"));
  const root = join(directory, "root");
  const coordinationDirectory = join(directory, "coordination");
  mkdirSync(root);
  mkdirSync(coordinationDirectory);
  const operationLock = join(
    coordinationDirectory,
    `${canonicalRootHash(realpathSync(root))}.operation.lock`,
  );
  const mutationLock = `${operationLock}.mutation`;
  mkdirSync(operationLock);
  const exited = Bun.spawn([process.execPath, "-e", ""], {
    stdout: "ignore",
    stderr: "ignore",
  });
  await exited.exited;
  writeFileSync(
    join(operationLock, "owner.json"),
    JSON.stringify({
      version: 1,
      operationId: crypto.randomUUID(),
      operation: "interrupted",
      root: realpathSync(root),
      rootHash: canonicalRootHash(realpathSync(root)),
      pid: exited.pid,
      startedAt: new Date().toISOString(),
    }),
  );
  mkdirSync(mutationLock);
  writeFileSync(
    join(mutationLock, "owner.json"),
    JSON.stringify({
      version: 1,
      token: crypto.randomUUID(),
      pid: process.pid,
    }),
  );
  const entered: string[] = [];
  let releaseFirst!: () => void;
  const firstHeld = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  const contender = (name: string) =>
    withRootOperation(
      root,
      name,
      async () => {
        entered.push(name);
        if (entered.length === 1) await firstHeld;
      },
      { coordinationDirectory },
    );

  try {
    const first = contender("first");
    const second = contender("second");
    await Bun.sleep(30);
    expect(entered).toEqual([]);
    rmSync(mutationLock, { recursive: true });
    while (entered.length === 0) await Bun.sleep(5);
    await Bun.sleep(30);
    expect(entered).toHaveLength(1);
    releaseFirst();
    await Promise.all([first, second]);
    expect(new Set(entered)).toEqual(new Set(["first", "second"]));
  } finally {
    releaseFirst();
    rmSync(directory, { recursive: true, force: true });
  }
});
