import { expect, test } from "bun:test";
import { runBackendGuardian } from "./backend-guardian";

type OwnedProcess = {
  process: Bun.Subprocess;
  exit(): void;
  cleanup(): Promise<void>;
};

async function ownedProcess(
  options: { ignoresTerm?: boolean } = {},
): Promise<OwnedProcess> {
  const child = Bun.spawn(
    [
      process.execPath,
      "-e",
      [
        `process.on("SIGTERM", () => ${options.ignoresTerm ? "{}" : "process.exit(0)"});`,
        'process.stdin.on("data", () => process.exit(0));',
        'process.stdout.write("ready\\n");',
      ].join(""),
    ],
    { stdin: "pipe", stdout: "pipe", stderr: "ignore" },
  );
  if (!child.stdin || typeof child.stdin === "number") {
    child.kill();
    throw new Error("Owned process did not expose stdin.");
  }
  if (!child.stdout || typeof child.stdout === "number") {
    child.kill();
    throw new Error("Owned process did not expose stdout.");
  }

  const reader = child.stdout.getReader();
  try {
    const { done, value } = await reader.read();
    if (done || !value || !new TextDecoder().decode(value).includes("ready")) {
      throw new Error("Owned process did not acknowledge readiness.");
    }
  } finally {
    reader.releaseLock();
  }

  return {
    process: child,
    exit() {
      child.stdin.write("exit\n");
    },
    async cleanup() {
      if (child.exitCode === null) child.kill("SIGKILL");
      await child.exited;
    },
  };
}

async function exitedGateway(): Promise<Bun.Subprocess> {
  const gateway = Bun.spawn([process.execPath, "-e", "process.exit(0)"]);
  await gateway.exited;
  return gateway;
}

test("reaps an owned backend when its gateway exited before guardian startup", async () => {
  const gateway = await exitedGateway();
  const backend = await ownedProcess();
  try {
    await expect(
      runBackendGuardian([String(gateway.pid), String(backend.process.pid)]),
    ).resolves.toBe(0);
    expect(backend.process.exitCode).toBe(0);
  } finally {
    await backend.cleanup();
  }
});

test("continues monitoring a live gateway until it exits", async () => {
  const gateway = await ownedProcess();
  const backend = await ownedProcess();
  try {
    const guardian = runBackendGuardian([
      String(gateway.process.pid),
      String(backend.process.pid),
    ]);

    expect(backend.process.exitCode).toBeNull();
    gateway.exit();

    await expect(guardian).resolves.toBe(0);
    expect(backend.process.exitCode).toBe(0);
  } finally {
    await Promise.all([gateway.cleanup(), backend.cleanup()]);
  }
});

test("confirms an owned TERM-ignoring backend has exited after SIGKILL", async () => {
  const gateway = await exitedGateway();
  const backend = await ownedProcess({ ignoresTerm: true });
  try {
    await expect(
      runBackendGuardian([String(gateway.pid), String(backend.process.pid)]),
    ).resolves.toBe(0);
    expect(backend.process.signalCode).toBe("SIGKILL");
  } finally {
    await backend.cleanup();
  }
});
