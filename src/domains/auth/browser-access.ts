import { chmod, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import {
  browserAccessConfigSchema,
  type BrowserAccessConfig,
  type DirectAccessRegistration,
} from "./browser-access-contract";

export * from "./browser-access-contract";

export function browserAccessConfigPath(root: string): string {
  return join(root, "ui-access.json");
}

export async function loadBrowserAccessConfig(
  root: string,
): Promise<BrowserAccessConfig | null> {
  let contents: string;
  try {
    contents = await readFile(browserAccessConfigPath(root), "utf8");
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return null;
    throw new Error("Unable to read browser access configuration.");
  }
  try {
    return browserAccessConfigSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid ui-access.json. Configure browser access again with the LocalBase CLI.",
    );
  }
}

export async function saveBrowserAccessConfig(
  root: string,
  input: BrowserAccessConfig,
): Promise<BrowserAccessConfig> {
  const config = browserAccessConfigSchema.parse(input);
  const path = browserAccessConfigPath(root);
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  try {
    await writeFile(temporary, `${JSON.stringify(config, null, 2)}\n`, {
      encoding: "utf8",
      flag: "wx",
      mode: 0o600,
    });
    await chmod(temporary, 0o600);
    await rename(temporary, path);
  } finally {
    await unlink(temporary).catch((error: unknown) => {
      if (!(
        error instanceof Error &&
        "code" in error &&
        error.code === "ENOENT"
      ))
        throw error;
    });
  }
  return config;
}

export async function disableBrowserAccess(root: string): Promise<boolean> {
  try {
    await unlink(browserAccessConfigPath(root));
    return true;
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT")
      return false;
    throw new Error("Unable to disable browser access.");
  }
}

export function upsertAccessRegistration(
  current: BrowserAccessConfig | null,
  input: {
    registration: DirectAccessRegistration;
    origin: string;
    permissions: BrowserAccessConfig["permissions"];
  },
): BrowserAccessConfig {
  const registrations =
    current?.provider.kind === "direct"
      ? current.provider.registrations.filter(
          (registration) => registration.id !== input.registration.id,
        )
      : [];
  registrations.push(input.registration);
  registrations.sort((left, right) => left.id.localeCompare(right.id));
  return browserAccessConfigSchema.parse({
    provider: { kind: "direct", registrations },
    origin: input.origin,
    permissions: input.permissions,
  });
}

export type AccessRegistrationRemoval =
  | Readonly<{ kind: "not-found" }>
  | Readonly<{ kind: "disabled" }>
  | Readonly<{ kind: "configured"; config: BrowserAccessConfig }>;

export function removeAccessRegistration(
  current: BrowserAccessConfig | null,
  registrationId: string,
): AccessRegistrationRemoval {
  if (current?.provider.kind !== "direct") return { kind: "not-found" };
  const registrations = current.provider.registrations.filter(
    (registration) => registration.id !== registrationId,
  );
  if (registrations.length === current.provider.registrations.length)
    return { kind: "not-found" };
  if (registrations.length === 0) return { kind: "disabled" };
  return {
    kind: "configured",
    config: browserAccessConfigSchema.parse({
      ...current,
      provider: { kind: "direct", registrations },
    }),
  };
}
