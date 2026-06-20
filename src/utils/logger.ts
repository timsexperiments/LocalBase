export class Logger {
  static getTimestamp(): string {
    return new Date().toISOString();
  }

  static info(prefix: string, message: string): void {
    console.log(`[${Logger.getTimestamp()}] [INFO] [${prefix}] ${message}`);
  }

  static warn(prefix: string, message: string): void {
    console.warn(`[${Logger.getTimestamp()}] [WARN] [${prefix}] ${message}`);
  }

  static error(prefix: string, message: string, err?: Error): void {
    console.error(`[${Logger.getTimestamp()}] [ERROR] [${prefix}] ${message}${err ? `: ${err.stack ?? err.message}` : ""}`);
  }

  static request(ip: string, method: string, path: string, status: number, durationMs: number): void {
    console.log(`[${Logger.getTimestamp()}] [INFO] [HTTP] ${ip} - ${method} ${path} -> ${status} (${durationMs.toFixed(1)}ms)`);
  }

  static pipeStream(stream: ReadableStream<Uint8Array>, name: string): void {
    const reader = stream.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    const read = async () => {
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() ?? "";
          for (const line of lines) {
            if (line.trim()) {
              console.log(`[${Logger.getTimestamp()}] [${name}] ${line}`);
            }
          }
        }
      } catch (err) {
        // Stream reading failed or closed
      }
    };
    read();
  }
}
