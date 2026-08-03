import packageJson from "../package.json";
import { z } from "zod";
import { LOCALBASE_VERSION } from "../src/version";

const PackageMetadataSchema = z
  .object({ version: z.string().min(1) })
  .passthrough();

function isCanonicalDecimal(value: string): boolean {
  if (value.length === 0 || (value.length > 1 && value[0] === "0")) {
    return false;
  }

  for (const character of value) {
    if (character < "0" || character > "9") {
      return false;
    }
  }

  return true;
}

export function validateRuntimeReleaseTag(runtimeTag: unknown): void {
  const tag = z.string().min(1).parse(runtimeTag);
  const prefix = "whisper-v";

  if (!tag.startsWith(prefix)) {
    throw new Error(
      `Runtime tag must use whisper-v<major>.<minor>.<patch>; received ${tag}.`,
    );
  }

  const components = tag.slice(prefix.length).split(".");
  if (
    components.length !== 3 ||
    components.some((component) => !isCanonicalDecimal(component))
  ) {
    throw new Error(
      `Runtime tag must use whisper-v<major>.<minor>.<patch>; received ${tag}.`,
    );
  }
}

export function validateReleasePreflight(
  gitTag: unknown,
  packageVersion: unknown,
): void {
  const tag = z.string().min(1).parse(gitTag);
  const version = z.string().min(1).parse(packageVersion);
  const expectedTag = `v${LOCALBASE_VERSION}`;

  if (tag !== expectedTag) {
    throw new Error(`Release tag must be ${expectedTag}; received ${tag}.`);
  }
  if (version !== LOCALBASE_VERSION) {
    throw new Error(
      `package.json version must be ${LOCALBASE_VERSION}; received ${version}.`,
    );
  }
}

export function runReleasePreflight(gitTag: unknown): void {
  const packageVersion = PackageMetadataSchema.parse(packageJson).version;
  validateReleasePreflight(gitTag, packageVersion);
  console.log(`Release preflight passed for ${gitTag}.`);
}

if (import.meta.main) {
  try {
    if (Bun.argv[2] === "--runtime-tag") {
      validateRuntimeReleaseTag(Bun.argv[3]);
      console.log(`Runtime release tag validation passed for ${Bun.argv[3]}.`);
    } else {
      runReleasePreflight(Bun.argv[2]);
    }
  } catch (error) {
    console.error(error);
    process.exit(1);
  }
}
