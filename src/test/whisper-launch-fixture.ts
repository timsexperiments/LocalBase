declare const __WHISPER_CAPABILITY__: string;
declare const __WHISPER_ARGS_PATH__: string;
export {};

const args = Bun.argv.slice(2);
if (args[0] === "--localbase-capabilities") {
  console.log(__WHISPER_CAPABILITY__);
} else {
  await Bun.write(__WHISPER_ARGS_PATH__, JSON.stringify(args));
}
