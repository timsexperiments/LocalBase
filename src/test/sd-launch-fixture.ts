declare const __SD_CAPABILITY__: string;
declare const __SD_ARGS_PATH__: string;
declare const __SD_ENVIRONMENT_PATH__: string;
export {};

const args = Bun.argv.slice(2);
if (args[0] === "--localbase-capabilities") {
  console.log(
    __SD_CAPABILITY__ === "oversized"
      ? "x".repeat(256 * 1024)
      : __SD_CAPABILITY__,
  );
} else {
  await Bun.write(__SD_ARGS_PATH__, `${args.join("\n")}\n`);
  if (__SD_ENVIRONMENT_PATH__) {
    await Bun.write(__SD_ENVIRONMENT_PATH__, process.env.LD_LIBRARY_PATH ?? "");
  }
  const keepAlive = setInterval(() => {}, 60_000);
  process.on("SIGTERM", () => {
    clearInterval(keepAlive);
    process.exit(0);
  });
}
