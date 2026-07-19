import {
  launchLlamaServer,
  launchWhisperServer,
  type LocalBaseConfig,
} from "../../manager";

export function printServeNextSteps(host: string, port: number): void {
  console.log(`\nOpenAI-compatible base URL: http://${host}:${port}/v1`);
  console.log("Example model list request:");
  console.log(`curl http://${host}:${port}/v1/models`);
  console.log("Example chat request:");
  console.log(
    `curl http://${host}:${port}/v1/chat/completions -H 'Content-Type: application/json' -d '{"model":"<your-model>","messages":[{"role":"user","content":"hello"}]}'`,
  );
  console.log(
    "Use `local-base keys create` to generate API keys for your clients/proxy layer.",
  );
  console.log(
    "Note: local-base orchestrates serving, but model inference is executed by llama-server/whisper-server runtimes.",
  );
}

export async function serveLlm(
  config: LocalBaseConfig,
  modelFile: string,
  host: string,
  port: number,
  ctxSize: number,
): Promise<number> {
  printServeNextSteps(host, port);
  return await launchLlamaServer(config, modelFile, host, port, ctxSize);
}

export async function serveStt(
  config: LocalBaseConfig,
  modelFile: string,
  host: string,
  port: number,
): Promise<number> {
  console.log(`STT endpoint: http://${host}:${port}`);
  console.log(
    "Note: local-base launches whisper-server; the runtime process serves inference traffic.",
  );
  return await launchWhisperServer(config, modelFile, host, port);
}
