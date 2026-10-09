export type ColorEnvironment = Readonly<Record<string, string | undefined>>;

export type ColorStream = Readonly<{ isTTY?: boolean }>;

export function shouldUseColor(
  stream: ColorStream,
  environment: ColorEnvironment = process.env,
): boolean {
  if (environment.FORCE_COLOR !== undefined)
    return environment.FORCE_COLOR !== "0";
  if (environment.NO_COLOR !== undefined && environment.NO_COLOR !== "")
    return false;
  return stream.isTTY === true;
}
