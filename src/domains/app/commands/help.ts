export function printHelp(): void {
  console.log(`local-base - Bun TypeScript local AI installer/manager\n
Usage:
  local-base [--defaults] [--all] [--config <file.toml>] [--root <path>] [--host <host>] [--port <n>] [--ctx-size <n>] [--stt-host <host>] [--stt-port <n>] [--startup-on-boot <true|false>] [--llm-models <id1,id2>] [--stt-models <id1,id2>] [--active-llm <id>] [--active-stt <id>] [--create-key <true|false>]
  local-base init [--root <path>]
  local-base configure [--defaults] [--all] [--config <file.toml>] [--root <path>] [--host <host>] [--port <n>] [--ctx-size <n>] [--stt-host <host>] [--stt-port <n>] [--startup-on-boot <true|false>] [--llm-models <id1,id2>] [--stt-models <id1,id2>] [--active-llm <id>] [--active-stt <id>] [--create-key <true|false>]
  local-base doctor [--json]
  local-base catalog [--kind llm|stt|tts|image|video|audio]
  local-base recommend [--kind llm|stt|tts|image|video|audio] [--vram <gb>]
  local-base installed [--kind llm|stt|tts|image|video|audio]
  local-base install <model_id>
  local-base serve [--host <host>] [--port <port>] [--llm <true|false>] [--stt <true|false>] [--tts <true|false>] [--image <true|false>] [--video <true|false>] [--llm-host <host>] [--llm-port <port>] [--stt-host <host>] [--stt-port <port>] [--ctx-size <tokens>] [--stt-path <path>] [--llm-model-file <file>] [--stt-model-file <file>] [--auth <true|false>] [--auth-mode <bearer|x-api-key|either>] [--tts-upstream <url>] [--image-upstream <url>] [--video-upstream <url>]
  local-base keys list
  local-base keys create [--name <label>] [--expires-days <n>]
  local-base keys revoke <key_id>
  local-base keys rotate <key_id>
  local-base reset [--root <path>] [--yes]
  local-base uninstall [--root <path>] [--yes]\n`);
}
