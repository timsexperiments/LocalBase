import { afterEach, expect, test } from "bun:test";
import {
  buildConfigureArgs,
  buildServeArgs,
  buildUninstallArgs,
  resolveSmokeTarget,
  transcribe,
} from "./runtime-smoke";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("builds smoke invocations for the current CLI contract", () => {
  const root = "/tmp/localbase-smoke";

  expect(buildConfigureArgs(root)).toEqual([
    "--root",
    root,
    "--non-interactive",
    "configure",
    "--defaults",
    "--stt-models",
    "whisper-tiny-en-q8_0",
    "--active-stt",
    "whisper-tiny-en-q8_0",
    "--no-create-key",
  ]);
  expect(buildServeArgs(root, 22_731, 22_732)).toEqual([
    "--root",
    root,
    "--non-interactive",
    "serve",
    "--host",
    "127.0.0.1",
    "--port",
    "22731",
    "--no-llm",
    "--stt",
    "--stt-host",
    "127.0.0.1",
    "--stt-port",
    "22732",
    "--no-image",
    "--no-auth",
    "--bypass-memory-check",
  ]);
  expect(buildConfigureArgs(root, "linux-arm64")).toEqual([
    "--root",
    root,
    "--non-interactive",
    "configure",
    "--defaults",
    "--stt-models",
    "",
    "--image-models",
    "",
    "--no-create-key",
  ]);
  expect(buildServeArgs(root, 22_731, 22_732, "macos-x64")).toEqual([
    "--root",
    root,
    "--non-interactive",
    "serve",
    "--host",
    "127.0.0.1",
    "--port",
    "22731",
    "--no-stt",
    "--llm-model-file",
    "runtime-smoke-llm.gguf",
    "--no-image",
    "--no-auth",
    "--bypass-memory-check",
  ]);
  expect(buildUninstallArgs(root)).toEqual([
    "--root",
    root,
    "--non-interactive",
    "uninstall",
    "--yes",
  ]);
});

test("accepts only release qualification targets", () => {
  expect(resolveSmokeTarget("macos-arm64")).toBe("macos-arm64");
  expect(resolveSmokeTarget("linux-x64")).toBe("linux-x64");
  expect(resolveSmokeTarget("macos-x64")).toBe("macos-x64");
  expect(resolveSmokeTarget("linux-arm64")).toBe("linux-arm64");
  expect(() => resolveSmokeTarget("windows-x64")).toThrow(
    "LOCALBASE_SMOKE_TARGET",
  );
});

function mockFetch(handler: (request: Request, attempt: number) => Response) {
  let attempts = 0;
  globalThis.fetch = async (input, _init) => {
    const request = new Request(input);
    attempts += 1;
    return handler(request, attempts);
  };
  return { attempts: () => attempts };
}

test("retries only the validated lazy-start response", async () => {
  const fixture = mockFetch((_request, attempt) =>
    attempt === 1
      ? Response.json(
          {
            error: {
              message:
                "STT service is currently restarting or unavailable. Please try again shortly.",
              type: "api_error",
              param: null,
              code: "service_unavailable",
            },
          },
          { status: 503, headers: { "Retry-After": "0" } },
        )
      : Response.json({ text: "" }),
  );

  await transcribe("http://gateway.test");

  expect(fixture.attempts()).toBe(2);
});

test("fails immediately for malformed or unexpected error responses", async () => {
  const cases = [
    {
      name: "malformed JSON",
      response: new Response("not json", { status: 503 }),
      message: "invalid JSON",
    },
    {
      name: "unexpected error code",
      response: Response.json(
        {
          error: {
            message: "upstream failed",
            type: "api_error",
            param: null,
            code: "upstream_error",
          },
        },
        { status: 503 },
      ),
      message: "code upstream_error",
    },
  ];

  for (const testCase of cases) {
    const fixture = mockFetch(() => testCase.response.clone());
    await expect(transcribe("http://gateway.test")).rejects.toThrow(
      testCase.message,
    );
    expect(fixture.attempts(), testCase.name).toBe(1);
  }
});

test("fails immediately for a malformed successful response", async () => {
  const fixture = mockFetch(() => Response.json({ text: 42 }));

  await expect(transcribe("http://gateway.test")).rejects.toThrow(
    "invalid response body",
  );
  expect(fixture.attempts()).toBe(1);
});
