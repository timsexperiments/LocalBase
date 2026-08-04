export const runtimeModalities = ["llm", "stt", "image"] as const;

export type RuntimeModality = (typeof runtimeModalities)[number];

export type RuntimeComponent = "llama-server" | "whisper-server" | "sd-server";

export const modalityComponents: Record<RuntimeModality, RuntimeComponent> = {
  llm: "llama-server",
  stt: "whisper-server",
  image: "sd-server",
};
