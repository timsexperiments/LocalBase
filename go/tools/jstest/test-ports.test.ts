// tools/test-ports.sh's test_port_pick, run in sh against small blocks. A port in use is held by a
// listener here, on a port the system gave out. A run that hangs is killed, and that's a failure.
//
//   bun test go/tools/jstest
import { afterEach, describe, expect, test } from "bun:test";

const script = new URL("../test-ports.sh", import.meta.url).pathname;
const held: { stop: (force?: boolean) => void }[] = [];
afterEach(() => { for (const l of held.splice(0)) l.stop(true); });

// pick runs test_port_pick once in the block given, for at most 5 s.
async function pick(block: string) {
  const p = Bun.spawn(["sh", "-c", `. "${script}"; test_port_pick`], { env: { ...process.env, LOCALBASE_TEST_PORTS: block }, stdout: "pipe", stderr: "pipe" });
  const timer = setTimeout(() => p.kill(9), 5000);
  const code = await p.exited;
  clearTimeout(timer);
  return { code, port: (await new Response(p.stdout).text()).trim(), said: (await new Response(p.stderr).text()).trim(), killed: p.signalCode !== null };
}

// hold listens on a port the system picks, and returns it.
function hold() {
  const l = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  held.push(l);
  return l.port;
}

// A port next to a held one that nothing listens on: tried by listening on it and letting go.
function freeNextTo(port: number) {
  for (const p of [port + 1, port - 1]) {
    try { Bun.listen({ hostname: "127.0.0.1", port: p, socket: { data() {} } }).stop(true); return p; } catch {}
  }
  return null;
}

describe("test_port_pick", () => {
  test("skips a port in use, every time", async () => {
    const taken = hold();
    const free = freeNextTo(taken);
    if (free === null) throw new Error("no free port next to the held one to test with");
    const block = `${Math.min(taken, free)}-${Math.max(taken, free)}`;
    for (let i = 0; i < 20; i++) {
      const r = await pick(block);
      expect(r).toEqual({ code: 0, port: String(free), said: "", killed: false });
    }
  });

  test("with every port in the block in use, it says so and fails, rather than waiting", async () => {
    const taken = hold();
    const began = Date.now();
    const r = await pick(`${taken}-${taken}`);
    expect(r.killed).toBe(false);
    expect(r.code).toBe(1);
    expect(r.port).toBe("");
    expect(r.said).toBe(`test_port_pick: no free port in ${taken}-${taken}`);
    expect(Date.now() - began).toBeLessThan(3000);
  });

  test("never gives 24173 or 25611-25613, even when nothing listens there", async () => {
    for (const block of ["24173-24173", "25611-25613"]) {
      const r = await pick(block);
      expect(r).toEqual({ code: 1, port: "", said: `test_port_pick: no free port in ${block}`, killed: false });
    }
  });

  test("gives the one free port of a block of one", async () => {
    const taken = hold();
    const free = freeNextTo(taken);
    if (free === null) throw new Error("no free port next to the held one to test with");
    expect(await pick(`${free}-${free}`)).toEqual({ code: 0, port: String(free), said: "", killed: false });
  });
});
