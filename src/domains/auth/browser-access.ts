import { chmod, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import {
  browserAccessConfigSchema,
  type BrowserAccessConfig,
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
    await rename(temporary, path);
    await chmod(path, 0o600);
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
