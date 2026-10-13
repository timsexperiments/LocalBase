export function reservePort(): number {
  const minPort = Number(process.env.LOCALBASE_TEST_PORT_MIN ?? 24_000);
  const maxPort = Number(process.env.LOCALBASE_TEST_PORT_MAX ?? 29_999);
  if (
    !Number.isInteger(minPort) ||
    !Number.isInteger(maxPort) ||
    minPort < 1 ||
    maxPort > 65_535 ||
    minPort > maxPort
  ) {
    throw new Error("Invalid LOCALBASE_TEST_PORT_MIN/MAX range.");
  }
  for (let attempt = 0; attempt < 20; attempt++) {
    const random = new Uint32Array(1);
    crypto.getRandomValues(random);
    const port = minPort + (random[0] % (maxPort - minPort + 1));
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
  throw new Error("Could not reserve a test port.");
}
