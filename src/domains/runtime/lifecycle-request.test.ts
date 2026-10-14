import { heapStats } from "bun:jsc";
import { describe, expect, test } from "bun:test";
import { withLifecycleSignal } from "./lifecycle-request";

function retained(): { protectedObjects: number; streams: number } {
  Bun.gc(true);
  const stats = heapStats();
  return {
    protectedObjects: stats.protectedObjectCount,
    streams: stats.objectTypeCounts.ReadableStream ?? 0,
  };
}

describe("withLifecycleSignal", () => {
  test("does not retain unread request bodies of rejected requests", async () => {
    using server = Bun.serve({
      port: 0,
      fetch(request) {
        const controller = new AbortController();
        withLifecycleSignal(
          request,
          AbortSignal.any([request.signal, controller.signal]),
        );
        return new Response("unauthorized", { status: 401 });
      },
    });
    const body = JSON.stringify({ content: "x".repeat(4096) });
    const send = async (count: number) => {
      for (let sent = 0; sent < count; sent += 20) {
        await Promise.all(
          Array.from({ length: 20 }, async () => {
            const response = await fetch(server.url, { method: "POST", body });
            await response.arrayBuffer();
          }),
        );
      }
      await Bun.sleep(50);
    };

    await send(100);
    const before = retained();
    await send(600);
    const after = retained();

    // The leaking form retained 2 protected objects and 3 streams per request.
    expect(after.protectedObjects - before.protectedObjects).toBeLessThan(100);
    expect(after.streams - before.streams).toBeLessThan(100);
  });

  test("keeps method, headers, body, and signal", async () => {
    const controller = new AbortController();
    const original = new Request("http://localhost/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", "x-test": "1" },
      body: '{"ok":true}',
    });
    const wrapped = withLifecycleSignal(original, controller.signal);
    expect(wrapped.method).toBe("POST");
    expect(wrapped.headers.get("x-test")).toBe("1");
    expect(await wrapped.clone().json()).toEqual({ ok: true });
    expect(await wrapped.text()).toBe('{"ok":true}');
    controller.abort();
    expect(wrapped.signal.aborted).toBe(true);
  });

  test("leaves bodyless requests without a body", () => {
    const wrapped = withLifecycleSignal(
      new Request("http://localhost/health"),
      new AbortController().signal,
    );
    expect(wrapped.method).toBe("GET");
    expect(wrapped.body).toBeNull();
  });
});
