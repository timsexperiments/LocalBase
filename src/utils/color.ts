export type ColorEnvironment = Readonly<Record<string, string | undefined>>;

export type ColorStream = Readonly<{ isTTY?: boolean }>;

export function shouldUseColor(
  stream: ColorStream,
  environment: ColorEnvironment = process.env,
): boolean {
  if (environment.NO_COLOR !== undefined && environment.NO_COLOR !== "")
    return false;
  if (environment.FORCE_COLOR !== undefined && environment.FORCE_COLOR !== "")
    return !["0", "false"].includes(environment.FORCE_COLOR.toLowerCase());
  if (environment.TERM === "dumb") return false;
  return stream.isTTY === true;
}

export function stripAnsiCodes(value: string): string {
  return value
    .replace(
      /\x1b\](?:[^\x07\x1b]|\x1b(?!\\))*(?:\x07|\x1b\\)|\u009d[^\u0007\u009c]*(?:\u0007|\u009c)|\x1b(?:\[[0-?]*[ -/]*[@-~]|[ -/]*[0-~])|\u009b[0-?]*[ -/]*[@-~]/g,
      "",
    )
    .replace(
      /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f\u0080-\u009f]/g,
      "",
    );
}
