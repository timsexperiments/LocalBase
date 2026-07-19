/**
 * Standard interface for LocalBase logging engines.
 */
export interface ILogger {
  info(prefix: string, message: string): void;
  warn(prefix: string, message: string): void;
  error(prefix: string, message: string, err?: Error): void;
  request(
    ip: string,
    method: string,
    path: string,
    status: number,
    durationMs: number,
  ): void;
  pipeStream(stream: ReadableStream<Uint8Array>, name: string): void;
}

/**
 * Colorized, human-readable terminal logger. Optimized for development.
 */
export class ConsoleLogger implements ILogger {
  private getTimestamp(): string {
    return new Date().toISOString();
  }

  info(prefix: string, message: string): void {
    console.log(
      `[${this.getTimestamp()}] \x1b[32m[INFO]\x1b[0m [\x1b[36m${prefix}\x1b[0m] ${message}`,
    );
  }

  warn(prefix: string, message: string): void {
    console.warn(
      `[${this.getTimestamp()}] \x1b[33m[WARN]\x1b[0m [\x1b[36m${prefix}\x1b[0m] ${message}`,
    );
  }

  error(prefix: string, message: string, err?: Error): void {
    const errText = err ? `: ${err.stack ?? err.message}` : "";
    console.error(
      `[${this.getTimestamp()}] \x1b[31m[ERROR]\x1b[0m [\x1b[36m${prefix}\x1b[0m] ${message}${errText}`,
    );
  }

  request(
    ip: string,
    method: string,
    path: string,
    status: number,
    durationMs: number,
  ): void {
    const color =
      status >= 500 ? "\x1b[31m" : status >= 400 ? "\x1b[33m" : "\x1b[32m";
    console.log(
      `[${this.getTimestamp()}] \x1b[32m[INFO]\x1b[0m [\x1b[35mHTTP\x1b[0m] ${ip} - ${method} ${path} -> ${color}${status}\x1b[0m (${durationMs.toFixed(1)}ms)`,
    );
  }

  pipeStream(stream: ReadableStream<Uint8Array>, name: string): void {
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
              console.log(
                `[${this.getTimestamp()}] [\x1b[36m${name}\x1b[0m] ${line}`,
              );
            }
          }
        }
      } catch (err) {
        // Stream closed or completed
      }
    };
    read();
  }
}

/**
 * Structured JSON-line logger. Optimized for production metrics and ship-to-service collectors.
 */
export class JsonLogger implements ILogger {
  private log(
    level: string,
    prefix: string,
    message: string,
    extra?: Record<string, any>,
  ): void {
    console.log(
      JSON.stringify({
        timestamp: new Date().toISOString(),
        level,
        prefix,
        message,
        ...extra,
      }),
    );
  }

  info(prefix: string, message: string): void {
    this.log("INFO", prefix, message);
  }

  warn(prefix: string, message: string): void {
    this.log("WARN", prefix, message);
  }

  error(prefix: string, message: string, err?: Error): void {
    this.log(
      "ERROR",
      prefix,
      message,
      err ? { error: err.message, stack: err.stack } : undefined,
    );
  }

  request(
    ip: string,
    method: string,
    path: string,
    status: number,
    durationMs: number,
  ): void {
    this.log("INFO", "HTTP", `${method} ${path} -> ${status}`, {
      http: {
        client_ip: ip,
        method,
        path,
        status,
        duration_ms: parseFloat(durationMs.toFixed(2)),
      },
    });
  }

  pipeStream(stream: ReadableStream<Uint8Array>, name: string): void {
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
              let parsed: any = null;
              try {
                parsed = JSON.parse(line);
              } catch (e) {
                // Not standard JSON
              }
              if (parsed) {
                // Merge structured logs from backend process directly
                console.log(
                  JSON.stringify({
                    timestamp: parsed.timestamp ?? new Date().toISOString(),
                    level: parsed.level ?? "INFO",
                    prefix: name,
                    message: parsed.message ?? line,
                    ...parsed,
                  }),
                );
              } else {
                this.log("INFO", name, line);
              }
            }
          }
        }
      } catch (err) {
        // Stream closed or completed
      }
    };
    read();
  }
}

/**
 * Factory function to instantiate console or structured JSON logger format.
 */
export function createLogger(format?: string): ILogger {
  if (
    format?.toLowerCase() === "json" ||
    process.env.LOG_FORMAT?.toLowerCase() === "json"
  ) {
    return new JsonLogger();
  }
  return new ConsoleLogger();
}
