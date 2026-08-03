import { join } from "node:path";

const ACTION_MAJOR_VERSIONS = new Map([
  ["actions/checkout", 4],
  ["actions/upload-artifact", 4],
  ["actions/download-artifact", 4],
  ["actions/attest-build-provenance", 3],
  ["oven-sh/setup-bun", 2],
  ["softprops/action-gh-release", 2],
]);

const SHA_LENGTH = 40;

type ActionReference = {
  reference: string;
  versionComment?: string;
  line: number;
};

function workflowActionReferences(value: unknown): string[] {
  if (Array.isArray(value)) {
    return value.flatMap(workflowActionReferences);
  }

  if (value === null || typeof value !== "object") return [];

  return Object.entries(value).flatMap(([key, child]) => [
    ...(key === "uses" && typeof child === "string" ? [child] : []),
    ...workflowActionReferences(child),
  ]);
}

function unquote(value: string): string {
  const quote = value.at(0);
  return quote && (quote === '"' || quote === "'") && value.at(-1) === quote
    ? value.slice(1, -1)
    : value;
}

function sourceActionReferences(source: string): ActionReference[] {
  return source.split("\n").flatMap((line, index) => {
    const trimmed = line.trimStart();
    const mapping = trimmed.startsWith("- ")
      ? trimmed.slice("- ".length).trimStart()
      : trimmed;
    if (!mapping.startsWith("uses:")) return [];

    const value = mapping.slice("uses:".length).trim();
    const commentIndex = value.indexOf("#");
    const reference = unquote(
      (commentIndex === -1 ? value : value.slice(0, commentIndex)).trim(),
    );
    const versionComment =
      commentIndex === -1 ? undefined : value.slice(commentIndex + 1).trim();

    return [{ reference, versionComment, line: index + 1 }];
  });
}

function isSha(value: string): boolean {
  return (
    value.length === SHA_LENGTH &&
    [...value].every((character) => "0123456789abcdef".includes(character))
  );
}

function validateActionReference(
  workflowPath: string,
  action: ActionReference,
): string[] {
  if (action.reference.startsWith("./")) return [];

  const separator = action.reference.lastIndexOf("@");
  const name = action.reference.slice(0, separator);
  const pin = action.reference.slice(separator + 1);
  const major = ACTION_MAJOR_VERSIONS.get(name);
  const location = `${workflowPath}:${action.line}`;

  if (separator <= 0 || !major) {
    return [`${location}: action ${action.reference} is not allowlisted`];
  }

  if (!isSha(pin)) {
    return [
      `${location}: action ${name} must use a 40-character lowercase SHA`,
    ];
  }

  if (action.versionComment !== `v${major}`) {
    return [`${location}: action ${name} must be annotated # v${major}`];
  }

  return [];
}

export function validateWorkflowSource(
  workflowPath: string,
  source: string,
): string[] {
  let workflow: unknown;
  try {
    workflow = Bun.YAML.parse(source);
  } catch (error) {
    return [`${workflowPath}: invalid YAML: ${String(error)}`];
  }

  const parsedReferences = workflowActionReferences(workflow);
  const sourceReferences = sourceActionReferences(source);
  if (parsedReferences.length !== sourceReferences.length) {
    return [
      `${workflowPath}: every uses entry must be a standalone uses: mapping`,
    ];
  }

  const sourceValues = sourceReferences.map(({ reference }) => reference);
  if (
    parsedReferences.some(
      (reference, index) => reference !== sourceValues[index],
    )
  ) {
    return [
      `${workflowPath}: could not associate uses entries with source lines`,
    ];
  }

  return sourceReferences.flatMap((action) =>
    validateActionReference(workflowPath, action),
  );
}

export async function validateWorkflowDirectory(
  workflowDirectory: string,
): Promise<string[]> {
  const patterns = ["*.yml", "*.yaml"];
  const paths = (
    await Promise.all(
      patterns.map(async (pattern) => {
        const glob = new Bun.Glob(pattern);
        return Array.from(
          glob.scan({ cwd: workflowDirectory, onlyFiles: true }),
        );
      }),
    )
  ).flat();

  return (
    await Promise.all(
      paths.sort().map(async (path) => {
        const workflowPath = join(workflowDirectory, path);
        return validateWorkflowSource(
          workflowPath,
          await Bun.file(workflowPath).text(),
        );
      }),
    )
  ).flat();
}
