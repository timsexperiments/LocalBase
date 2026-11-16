// The Kokoro runtime's own dependencies (runtimes/kokoro/package.json). They're installed only into
// dist/kokoro/package by release-kokoro.yml, whose `tsgo --project scripts/runtimes/kokoro-runtime-tsconfig.json`
// checks runtimes/kokoro against their real types. The root typecheck reaches runtimes/kokoro/cli.ts
// through the Kokoro release test, without those packages, so it sees them untyped. The Kokoro
// tsconfig doesn't include this file.
declare module "@huggingface/transformers";
declare module "kokoro-js";
declare module "onnxruntime-node";
declare module "phonemizer";
