import { expect, test } from "bun:test";
import { printHelp } from "./help";

test("help documents configuration and image-serving controls", () => {
  const output: string[] = [];
  const originalLog = console.log;
  console.log = (message?: unknown) => output.push(String(message));

  try {
    printHelp();
  } finally {
    console.log = originalLog;
  }

  const help = output.join("\n");
  expect(help).toContain("[--parallel <n|auto>]");
  expect(help).toContain("[--image-models <id1,id2>]");
  expect(help).toContain("[--image-host <host>]");
  expect(help).toContain("[--image-model-file <file>]");
});
