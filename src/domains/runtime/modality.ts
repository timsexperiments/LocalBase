export const runtimeModalities = ["llm", "stt", "tts", "image"] as const;

export type RuntimeModality = (typeof runtimeModalities)[number];

export type RuntimeComponent =
  "llama-server" | "whisper-server" | "llama-tts" | "sd-server";

export const modalityComponents: Record<RuntimeModality, RuntimeComponent> = {
  llm: "llama-server",
  stt: "whisper-server",
  tts: "llama-tts",
  image: "sd-server",
};
