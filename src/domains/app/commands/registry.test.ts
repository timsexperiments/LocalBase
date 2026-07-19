import { expect, test } from "bun:test";
import { printHelp } from "./help";

test("default and configure help advertise parallel slots", () => {
  const output: string[] = [];
  const originalLog = console.log;
  console.log = (message?: unknown) => output.push(String(message));

  try {
    printHelp();
  } finally {
    console.log = originalLog;
  }

  const help = output.join("\n");
  expect(help).toMatch(/local-base .*\[--parallel <n\|auto>]\s+\[--stt-host/);
  expect(help).toMatch(
    /local-base configure .*\[--parallel <n\|auto>]\s+\[--stt-host/,
  );
});
