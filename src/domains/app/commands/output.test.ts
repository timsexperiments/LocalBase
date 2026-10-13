import { expect, test } from "bun:test";
import { join } from "node:path";

test("large JSON output drains completely before a child exits", async () => {
  const child = Bun.spawn(
    [
      process.execPath,
      "--eval",
      `import { writeJsonSuccess } from "./src/domains/app/commands/output.ts"; const payload = Array.from({ length: 7000 }, (_, index) => ({ id: "key-" + index, name: "key " + index, description: "x".repeat(200), createdAt: "2026-10-09T00:00:00.000Z", scopes: ["inference:chat", "inference:embeddings"] })); writeJsonSuccess(payload); process.exitCode = 0;`,
    ],
    {
      cwd: join(import.meta.dirname, "../../../.."),
      stdout: "pipe",
      stderr: "pipe",
    },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);

  expect(exitCode).toBe(0);
  expect(stderr).toBe("");
  expect(Buffer.byteLength(stdout)).toBeGreaterThan(1_300_000);
  const parsed = JSON.parse(stdout) as { data: { id: string }[] };
  expect(parsed.data).toHaveLength(7_000);
  expect(parsed.data.at(-1)?.id).toBe("key-6999");
});
