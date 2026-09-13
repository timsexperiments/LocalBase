declare const __WHISPER_CAPABILITY__: string;
declare const __WHISPER_ARGS_PATH__: string;
export {};

const args = Bun.argv.slice(2);
if (args[0] === "--localbase-capabilities") {
  if (__WHISPER_CAPABILITY__ === "hang") {
    await Bun.write(__WHISPER_ARGS_PATH__, String(process.pid));
    process.on("SIGTERM", () => {});
    setInterval(() => {}, 60_000);
  } else {
    console.log(
      __WHISPER_CAPABILITY__ === "oversized"
        ? "x".repeat(256 * 1024)
        : __WHISPER_CAPABILITY__,
    );
  }
} else {
  await Bun.write(__WHISPER_ARGS_PATH__, JSON.stringify(args));
}
