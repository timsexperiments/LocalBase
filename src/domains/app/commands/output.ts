export interface CommandOutput {
  info(message: string): void;
  error(message: string): void;
}

export function createCommandOutput(): CommandOutput {
  return {
    info(message) {
      console.log(message);
    },
    error(message) {
      console.error(message);
    },
  };
}
