import { realpath } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { z } from "zod";
import { CliInputError } from "../app/commands/errors";
import { canonicalRoot, canonicalRootHash } from "./ownership";
import {
  OTEL_ENVIRONMENT_NAMES,
  otelServiceEnvironmentSchema,
} from "../observability/otel-config";

export const servicePlatformSchema = z.enum(["darwin", "linux"]);
export type ServicePlatform = z.infer<typeof servicePlatformSchema>;

const serviceIdSchema = z
  .string()
  .regex(/^com\.localbase\.gateway\.[a-f0-9]{64}$/);

export const serviceInstanceTokenSchema = z.uuid();

const servicePathSchema = z
  .string()
  .min(1)
  .refine(isAbsolute, "must be an absolute path")
  .refine((value) => resolve(value) === value, "must be normalized")
  .refine((value) => !/[\u0000\r\n]/.test(value), {
    message: "must not contain NUL or line breaks",
  });

const serviceArgumentSchema = z
  .string()
  .min(1)
  .refine(
    (value) => !/[\u0000\r\n]/.test(value),
    "must not contain NUL or line breaks",
  );

export const serviceInvocationSchema = z
  .object({
    program: servicePathSchema,
    arguments: z.array(serviceArgumentSchema).min(1),
    serviceToken: serviceInstanceTokenSchema.optional(),
  })
  .strict();

export type ServiceInvocation = z.infer<typeof serviceInvocationSchema>;

export const serviceMetadataSchema = z
  .object({
    platform: servicePlatformSchema,
    manager: z.enum(["launchd", "systemd-user"]),
    serviceId: serviceIdSchema,
    unitName: z.string().regex(/^local-base-gateway-[a-f0-9]{64}\.service$/),
    definitionPath: servicePathSchema,
  })
  .strict();

export type ServiceMetadata = z.infer<typeof serviceMetadataSchema>;

const serviceEnvironmentSchema = z
  .object({
    LOCALBASE_SERVICE_ID: serviceIdSchema,
    LOCALBASE_SERVICE_TOKEN: serviceInstanceTokenSchema,
  })
  .catchall(z.string())
  .superRefine((value, ctx) => {
    const telemetry = Object.fromEntries(
      Object.entries(value).filter(([name]) => name.startsWith("OTEL_")),
    );
    const parsed = otelServiceEnvironmentSchema.safeParse(telemetry);
    if (!parsed.success) {
      for (const issue of parsed.error.issues) {
        ctx.addIssue({
          code: "custom",
          path: issue.path,
          message: issue.message,
        });
      }
    }
  });

export const launchdDefinitionSchema = z
  .object({
    label: serviceIdSchema,
    program: servicePathSchema,
    programArguments: z.array(serviceArgumentSchema).min(1),
    workingDirectory: servicePathSchema,
    runAtLoad: z.literal(true),
    keepAlive: z.literal(true),
    exitTimeOut: z.literal(15),
    umask: z.literal(63),
    processType: z.literal("Interactive"),
    environment: serviceEnvironmentSchema,
    standardOutPath: servicePathSchema,
    standardErrorPath: servicePathSchema,
  })
  .strict();

export const systemdDefinitionSchema = z
  .object({
    description: z.literal("LocalBase gateway"),
    type: z.literal("exec"),
    execStart: z.array(serviceArgumentSchema).min(1),
    workingDirectory: servicePathSchema,
    restart: z.literal("on-failure"),
    restartSec: z.literal("2s"),
    killMode: z.literal("mixed"),
    killSignal: z.literal("SIGTERM"),
    timeoutStopSec: z.literal("15s"),
    umask: z.literal("0077"),
    environment: serviceEnvironmentSchema,
    standardOutput: z.literal("journal"),
    standardError: z.literal("journal"),
    wantedBy: z.literal("default.target"),
  })
  .strict();

export const serviceManifestSchema = z
  .object({
    version: z.literal(1),
    root: servicePathSchema,
    rootHash: z.string().regex(/^[a-f0-9]{64}$/),
    manager: z.enum(["launchd", "systemd-user"]),
    serviceId: serviceIdSchema,
    definitionPath: servicePathSchema,
    definitionFingerprint: z.string().regex(/^[a-f0-9]{64}$/),
    serviceToken: serviceInstanceTokenSchema,
    invocation: serviceInvocationSchema,
  })
  .strict();

export type ServiceManifest = z.infer<typeof serviceManifestSchema>;

export type ServiceDefinition = ServiceMetadata & {
  contents: string;
  fingerprint: string;
  serviceToken: string;
  invocation: ServiceInvocation;
  manifest: ServiceManifest;
};

function serviceRoot(root: string): string {
  const parsed = servicePathSchema.safeParse(root);
  if (!parsed.success) {
    throw new CliInputError(
      `Service management requires a normalized LocalBase root without NUL or line breaks: ${parsed.error.issues[0]?.message ?? "invalid root"}.`,
    );
  }
  return parsed.data;
}

function userHome(): string {
  return servicePathSchema.parse(resolve(process.env.HOME || homedir()));
}

function linuxConfigHome(): string {
  const candidate = process.env.XDG_CONFIG_HOME || join(userHome(), ".config");
  return servicePathSchema.parse(resolve(candidate));
}

function runtimePlatform(): string {
  return process["platform"];
}

export function servicePlatform(platform = runtimePlatform()): ServicePlatform {
  const parsed = servicePlatformSchema.safeParse(platform);
  if (parsed.success) return parsed.data;
  throw new Error(
    `User service management is supported on macOS (launchd) and Linux (systemd --user); use "local-base serve" in the foreground on ${platform}.`,
  );
}

export async function serviceIdentity(root: string): Promise<string> {
  const canonical = await canonicalRoot(root);
  return `com.localbase.gateway.${canonicalRootHash(canonical)}`;
}

export async function serviceMetadata(
  root: string,
  platform = servicePlatform(),
): Promise<ServiceMetadata> {
  const normalizedRoot = await canonicalRoot(root);
  const serviceId = await serviceIdentity(normalizedRoot);
  const unitName = `local-base-gateway-${serviceId.slice("com.localbase.gateway.".length)}.service`;
  const definitionPath =
    platform === "darwin"
      ? join(userHome(), "Library", "LaunchAgents", `${serviceId}.plist`)
      : join(linuxConfigHome(), "systemd", "user", unitName);

  return serviceMetadataSchema.parse({
    platform,
    manager: platform === "darwin" ? "launchd" : "systemd-user",
    serviceId,
    unitName,
    definitionPath: resolve(definitionPath),
  });
}

function xmlEscape(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

function xmlUnescape(value: string): string {
  return value
    .replaceAll("&apos;", "'")
    .replaceAll("&quot;", '"')
    .replaceAll("&gt;", ">")
    .replaceAll("&lt;", "<")
    .replaceAll("&amp;", "&");
}

function plistString(xml: string, key: string): string | undefined {
  const match = xml.match(
    new RegExp(`<key>${key}</key>\\s*<string>([\\s\\S]*?)</string>`),
  );
  return match ? xmlUnescape(match[1]) : undefined;
}

function plistInteger(xml: string, key: string): number | undefined {
  const value = xml.match(
    new RegExp(`<key>${key}</key>\\s*<integer>(\\d+)</integer>`),
  )?.[1];
  return value === undefined ? undefined : Number(value);
}

function plistBoolean(xml: string, key: string): boolean {
  return new RegExp(`<key>${key}</key>\\s*<true\\s*/>`).test(xml);
}

function plistArray(xml: string, key: string): string[] | undefined {
  const match = xml.match(
    new RegExp(`<key>${key}</key>\\s*<array>([\\s\\S]*?)</array>`),
  );
  if (!match) return undefined;
  const values = [...match[1].matchAll(/<string>([\s\S]*?)<\/string>/g)].map(
    (entry) => xmlUnescape(entry[1]),
  );
  return values.length > 0 ? values : undefined;
}

function plistEnvironment(xml: string): unknown {
  const match = xml.match(
    /<key>EnvironmentVariables<\/key>\s*<dict>([\s\S]*?)<\/dict>/,
  );
  const required = {
    LOCALBASE_SERVICE_ID: match
      ? plistString(match[1], "LOCALBASE_SERVICE_ID")
      : undefined,
    LOCALBASE_SERVICE_TOKEN: match
      ? plistString(match[1], "LOCALBASE_SERVICE_TOKEN")
      : undefined,
  };
  const telemetry = Object.fromEntries(
    OTEL_ENVIRONMENT_NAMES.flatMap((name) => {
      const value = match ? plistString(match[1], name) : undefined;
      return value === undefined ? [] : [[name, value]];
    }),
  );
  return { ...required, ...telemetry };
}

export function renderLaunchdDefinition(
  metadata: ServiceMetadata,
  invocation: ServiceInvocation,
  root: string,
  serviceToken: string,
  otelEnvironment: Record<string, string> = {},
): string {
  const parsedMetadata = serviceMetadataSchema.parse(metadata);
  const parsedInvocation = serviceInvocationSchema.parse(invocation);
  const workingDirectory = serviceRoot(root);
  const programArguments = [
    parsedInvocation.program,
    ...parsedInvocation.arguments,
  ];
  const stdoutPath = "/dev/null";
  const stderrPath = "/dev/null";
  const telemetry = otelServiceEnvironmentSchema.parse(otelEnvironment);

  launchdDefinitionSchema.parse({
    label: parsedMetadata.serviceId,
    program: parsedInvocation.program,
    programArguments,
    workingDirectory,
    runAtLoad: true,
    keepAlive: true,
    exitTimeOut: 15,
    umask: 63,
    processType: "Interactive",
    environment: {
      LOCALBASE_SERVICE_ID: parsedMetadata.serviceId,
      LOCALBASE_SERVICE_TOKEN: serviceInstanceTokenSchema.parse(serviceToken),
      ...telemetry,
    },
    standardOutPath: stdoutPath,
    standardErrorPath: stderrPath,
  });

  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    "<dict>",
    `  <key>Label</key><string>${xmlEscape(parsedMetadata.serviceId)}</string>`,
    `  <key>Program</key><string>${xmlEscape(parsedInvocation.program)}</string>`,
    "  <key>ProgramArguments</key>",
    "  <array>",
    ...programArguments.map(
      (argument) => `    <string>${xmlEscape(argument)}</string>`,
    ),
    "  </array>",
    `  <key>WorkingDirectory</key><string>${xmlEscape(workingDirectory)}</string>`,
    "  <key>RunAtLoad</key><true/>",
    "  <key>KeepAlive</key><true/>",
    "  <key>ExitTimeOut</key><integer>15</integer>",
    "  <key>Umask</key><integer>63</integer>",
    "  <key>ProcessType</key><string>Interactive</string>",
    "  <key>EnvironmentVariables</key>",
    "  <dict>",
    `    <key>LOCALBASE_SERVICE_ID</key><string>${xmlEscape(parsedMetadata.serviceId)}</string>`,
    `    <key>LOCALBASE_SERVICE_TOKEN</key><string>${xmlEscape(serviceToken)}</string>`,
    ...Object.entries(telemetry)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(
        ([name, value]) =>
          `    <key>${xmlEscape(name)}</key><string>${xmlEscape(value)}</string>`,
      ),
    "  </dict>",
    `  <key>StandardOutPath</key><string>${xmlEscape(stdoutPath)}</string>`,
    `  <key>StandardErrorPath</key><string>${xmlEscape(stderrPath)}</string>`,
    "</dict>",
    "</plist>",
    "",
  ].join("\n");
}

export function parseLaunchdDefinition(contents: string) {
  const result = launchdDefinitionSchema.safeParse({
    label: plistString(contents, "Label"),
    program: plistString(contents, "Program"),
    programArguments: plistArray(contents, "ProgramArguments"),
    workingDirectory: plistString(contents, "WorkingDirectory"),
    runAtLoad: plistBoolean(contents, "RunAtLoad"),
    keepAlive: plistBoolean(contents, "KeepAlive"),
    exitTimeOut: plistInteger(contents, "ExitTimeOut"),
    umask: plistInteger(contents, "Umask"),
    processType: plistString(contents, "ProcessType"),
    environment: plistEnvironment(contents),
    standardOutPath: plistString(contents, "StandardOutPath"),
    standardErrorPath: plistString(contents, "StandardErrorPath"),
  });
  if (!result.success) {
    throw new Error(
      `Invalid launchd definition: ${result.error.issues[0]?.message ?? "malformed plist"}.`,
    );
  }
  return result.data;
}

function assertSystemdValue(value: string): void {
  if (/[\u0000-\u0008\u000a-\u001f\u007f]/.test(value)) {
    throw new Error(
      "systemd service values must not contain control characters other than tabs",
    );
  }
}

function quoteSystemdValue(value: string, expandDollar: boolean): string {
  assertSystemdValue(value);
  let encoded = '"';
  for (const character of value) {
    if (character === "\\") encoded += "\\\\";
    else if (character === '"') encoded += '\\"';
    else if (character === "\t") encoded += "\\t";
    else if (character === "%") encoded += "%%";
    else if (character === "$" && expandDollar) encoded += "$$";
    else encoded += character;
  }
  return `${encoded}"`;
}

function escapeSystemdPath(value: string): string {
  assertSystemdValue(value);
  let encoded = "";
  for (const character of value) {
    if (character === "%") encoded += "%%";
    else if (character === " ") encoded += "\\x20";
    else if (character === "\t") encoded += "\\x09";
    else if (character === '"') encoded += "\\x22";
    else if (character === "\\") encoded += "\\x5c";
    else encoded += character;
  }
  return encoded;
}

function parseSystemdPath(value: string): string | undefined {
  let decoded = "";
  for (let index = 0; index < value.length; index += 1) {
    const character = value[index];
    if (character === "%") {
      if (value[index + 1] !== "%") return undefined;
      decoded += "%";
      index += 1;
      continue;
    }
    if (character === "\\") {
      if (value[index + 1] !== "x") return undefined;
      const hexadecimal = value.slice(index + 2, index + 4);
      if (!/^[a-fA-F0-9]{2}$/.test(hexadecimal)) return undefined;
      decoded += String.fromCharCode(Number.parseInt(hexadecimal, 16));
      index += 3;
      continue;
    }
    decoded += character;
  }
  return decoded;
}

function parseSystemdArguments(
  value: string,
  expandDollar: boolean,
): string[] | undefined {
  const values: string[] = [];
  let index = 0;
  while (index < value.length) {
    while (value[index] === " " || value[index] === "\t") index += 1;
    if (index >= value.length) break;
    if (value[index] !== '"') return undefined;
    index += 1;
    let decoded = "";
    let closed = false;
    while (index < value.length) {
      const character = value[index];
      if (character === '"') {
        index += 1;
        closed = true;
        break;
      }
      if (character === "\\") {
        const escaped = value[index + 1];
        if (escaped === "\\" || escaped === '"') decoded += escaped;
        else if (escaped === "t") decoded += "\t";
        else return undefined;
        index += 2;
        continue;
      }
      if (character === "%") {
        if (value[index + 1] !== "%") return undefined;
        decoded += "%";
        index += 2;
        continue;
      }
      if (character === "$" && expandDollar) {
        if (value[index + 1] !== "$") return undefined;
        decoded += "$";
        index += 2;
        continue;
      }
      decoded += character;
      index += 1;
    }
    if (!closed) return undefined;
    if (index < value.length && value[index] !== " " && value[index] !== "\t") {
      return undefined;
    }
    values.push(decoded);
  }
  return values.length > 0 ? values : undefined;
}

function systemdValue(contents: string, key: string): string | undefined {
  return contents.match(new RegExp(`^${key}=(.*)$`, "m"))?.[1];
}

function systemdValues(contents: string, key: string): string[] {
  return [...contents.matchAll(new RegExp(`^${key}=(.*)$`, "gm"))].map(
    (match) => match[1],
  );
}

function systemdEnvironment(contents: string): unknown {
  const values = systemdValues(contents, "Environment").flatMap(
    (value) => parseSystemdArguments(value, false) ?? [],
  );
  if (values.length === 0) return {};
  return Object.fromEntries(
    values.flatMap((value) => {
      const separator = value.indexOf("=");
      return separator === -1
        ? []
        : [[value.slice(0, separator), value.slice(separator + 1)]];
    }),
  );
}

export function renderSystemdDefinition(
  metadata: ServiceMetadata,
  invocation: ServiceInvocation,
  root: string,
  serviceToken: string,
  otelEnvironment: Record<string, string> = {},
): string {
  const parsedMetadata = serviceMetadataSchema.parse(metadata);
  const parsedInvocation = serviceInvocationSchema.parse(invocation);
  const workingDirectory = serviceRoot(root);
  const execStart = [parsedInvocation.program, ...parsedInvocation.arguments];
  const telemetry = otelServiceEnvironmentSchema.parse(otelEnvironment);

  systemdDefinitionSchema.parse({
    description: "LocalBase gateway",
    type: "exec",
    execStart,
    workingDirectory,
    restart: "on-failure",
    restartSec: "2s",
    killMode: "mixed",
    killSignal: "SIGTERM",
    timeoutStopSec: "15s",
    umask: "0077",
    environment: {
      LOCALBASE_SERVICE_ID: parsedMetadata.serviceId,
      LOCALBASE_SERVICE_TOKEN: serviceInstanceTokenSchema.parse(serviceToken),
      ...telemetry,
    },
    standardOutput: "journal",
    standardError: "journal",
    wantedBy: "default.target",
  });

  return [
    "[Unit]",
    "Description=LocalBase gateway",
    "After=network.target",
    "",
    "[Service]",
    "Type=exec",
    `ExecStart=${execStart.map((value) => quoteSystemdValue(value, true)).join(" ")}`,
    `WorkingDirectory=${escapeSystemdPath(workingDirectory)}`,
    "Restart=on-failure",
    "RestartSec=2s",
    "KillMode=mixed",
    "KillSignal=SIGTERM",
    "TimeoutStopSec=15s",
    "UMask=0077",
    `Environment=${quoteSystemdValue(`LOCALBASE_SERVICE_ID=${parsedMetadata.serviceId}`, false)}`,
    `Environment=${quoteSystemdValue(`LOCALBASE_SERVICE_TOKEN=${serviceToken}`, false)}`,
    ...Object.entries(telemetry)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(
        ([name, value]) =>
          `Environment=${quoteSystemdValue(`${name}=${value}`, false)}`,
      ),
    "StandardOutput=journal",
    "StandardError=journal",
    "",
    "[Install]",
    "WantedBy=default.target",
    "",
  ].join("\n");
}

export function parseSystemdDefinition(contents: string) {
  const result = systemdDefinitionSchema.safeParse({
    description: systemdValue(contents, "Description"),
    type: systemdValue(contents, "Type"),
    execStart: parseSystemdArguments(
      systemdValue(contents, "ExecStart") ?? "",
      true,
    ),
    workingDirectory: parseSystemdPath(
      systemdValue(contents, "WorkingDirectory") ?? "",
    ),
    restart: systemdValue(contents, "Restart"),
    restartSec: systemdValue(contents, "RestartSec"),
    killMode: systemdValue(contents, "KillMode"),
    killSignal: systemdValue(contents, "KillSignal"),
    timeoutStopSec: systemdValue(contents, "TimeoutStopSec"),
    umask: systemdValue(contents, "UMask"),
    environment: systemdEnvironment(contents),
    standardOutput: systemdValue(contents, "StandardOutput"),
    standardError: systemdValue(contents, "StandardError"),
    wantedBy: systemdValue(contents, "WantedBy"),
  });
  if (!result.success) {
    throw new Error(
      `Invalid systemd definition: ${result.error.issues[0]?.message ?? "malformed unit"}.`,
    );
  }
  return result.data;
}

export async function createServiceDefinition(
  root: string,
  invocation: ServiceInvocation,
  platform = servicePlatform(),
  serviceToken: string = crypto.randomUUID(),
  otelEnvironment: Record<string, string> = {},
): Promise<ServiceDefinition> {
  const canonical = await canonicalRoot(root);
  const metadata = await serviceMetadata(canonical, platform);
  const parsedInvocation = serviceInvocationSchema.parse(invocation);
  const boundInvocation = serviceInvocationSchema.parse({
    ...parsedInvocation,
    serviceToken,
  });
  const contents =
    platform === "darwin"
      ? renderLaunchdDefinition(
          metadata,
          parsedInvocation,
          canonical,
          serviceToken,
          otelEnvironment,
        )
      : renderSystemdDefinition(
          metadata,
          parsedInvocation,
          canonical,
          serviceToken,
          otelEnvironment,
        );
  const fingerprint = new Bun.CryptoHasher("sha256")
    .update(contents)
    .digest("hex");
  const manifest = serviceManifestSchema.parse({
    version: 1,
    root: canonical,
    rootHash: canonicalRootHash(canonical),
    manager: metadata.manager,
    serviceId: metadata.serviceId,
    definitionPath: metadata.definitionPath,
    definitionFingerprint: fingerprint,
    serviceToken: serviceInstanceTokenSchema.parse(serviceToken),
    invocation: boundInvocation,
  });
  return {
    ...metadata,
    contents,
    fingerprint,
    serviceToken: manifest.serviceToken,
    invocation: boundInvocation,
    manifest,
  };
}

export async function resolveServiceInvocation(
  root: string,
): Promise<ServiceInvocation> {
  const canonical = await canonicalRoot(root);
  const program = servicePathSchema.parse(await realpath(process.execPath));
  const programFile = Bun.file(program);
  if (!(await programFile.stat()).isFile()) {
    throw new Error(`LocalBase executable is not a file: ${program}.`);
  }

  const entrypoint = Bun.main.startsWith("/$bunfs/")
    ? []
    : [servicePathSchema.parse(await realpath(Bun.main))];
  for (const path of entrypoint) {
    if (!(await Bun.file(path).stat()).isFile()) {
      throw new Error(`LocalBase entrypoint is not a file: ${path}.`);
    }
  }

  return serviceInvocationSchema.parse({
    program,
    arguments: [...entrypoint, "--root", canonical, "serve"],
  });
}

export function serviceManifestPath(root: string): string {
  return join(serviceRoot(root), "service", "manifest.json");
}

export function serviceDefinitionDirectory(
  definition: Pick<ServiceMetadata, "definitionPath">,
): string {
  return dirname(servicePathSchema.parse(definition.definitionPath));
}
