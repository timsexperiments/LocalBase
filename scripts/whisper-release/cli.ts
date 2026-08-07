import { defineCommand, runCommand, type ArgsDef } from "citty";
import { z } from "zod";
import { qualifyWhisperArchive } from "./archive";
import {
  filePathSchema,
  repositorySchema,
  teamIdSchema,
  validateWhisperReleaseTag,
  whisperTagSchema,
  whisperTargetSchema,
} from "./contracts";
import {
  assertWhisperReleaseAvailable,
  verifyPublishedWhisperRelease,
} from "./published-release";
import {
  updateWhisperManifestFile,
  verifyWhisperManifestFile,
  whisperManifestStatus,
} from "./manifest";

const githubOutputEntrySchema = z
  .object({
    name: z.string().regex(/^[A-Za-z_][A-Za-z0-9_]*$/),
    value: z.string().regex(/^[A-Za-z0-9._/-]+$/),
  })
  .strict();

async function appendGithubOutput(
  pathInput: unknown,
  entriesInput: Array<{ name: string; value: string }>,
): Promise<void> {
  const path = filePathSchema.parse(pathInput);
  const entries = z.array(githubOutputEntrySchema).parse(entriesInput);
  const file = Bun.file(path);
  const current = (await file.exists()) ? await file.text() : "";
  await Bun.write(
    path,
    `${current}${entries.map(({ name, value }) => `${name}=${value}\n`).join("")}`,
  );
}

async function writeValidatedTagOutput(
  tagInput: unknown,
  outputPathInput: unknown,
): Promise<void> {
  const tag = validateWhisperReleaseTag(tagInput);
  await appendGithubOutput(outputPathInput, [
    { name: "tag", value: tag },
    { name: "branch", value: `tim/${tag}-manifest` },
  ]);
}

const commandInputSchemas = {
  "validate-tag": z
    .object({ tag: whisperTagSchema, githubOutput: filePathSchema.optional() })
    .strict(),
  "assert-release-available": z
    .object({ repository: repositorySchema, tag: whisperTagSchema })
    .strict(),
  "qualify-archive": z
    .object({
      target: whisperTargetSchema,
      archive: filePathSchema,
      workDirectory: filePathSchema,
      teamId: teamIdSchema.optional(),
    })
    .strict(),
  "verify-published-release": z
    .object({
      repository: repositorySchema,
      tag: whisperTagSchema,
      output: filePathSchema,
    })
    .strict(),
  "update-manifest": z
    .object({ manifest: filePathSchema, receipt: filePathSchema })
    .strict(),
  "verify-manifest": z
    .object({ manifest: filePathSchema, receipt: filePathSchema })
    .strict(),
  "manifest-status": z
    .object({
      manifest: filePathSchema,
      receipt: filePathSchema,
      githubOutput: filePathSchema,
    })
    .strict(),
};

function parseCittyInput<T extends z.ZodType>(
  schema: T,
  args: Record<string, unknown>,
): z.infer<T> {
  const { _, ...input } = args;
  z.array(z.string()).length(0).parse(_);
  const normalized = new Map<string, unknown>();
  for (const [name, value] of Object.entries(input)) {
    const camelCaseName = name.replace(/-([a-z])/g, (_, letter: string) =>
      letter.toUpperCase(),
    );
    if (
      normalized.has(camelCaseName) &&
      normalized.get(camelCaseName) !== value
    ) {
      throw new Error(`Conflicting values for --${name}.`);
    }
    normalized.set(camelCaseName, value);
  }
  return schema.parse(Object.fromEntries(normalized));
}

const stringArgument = (description: string) =>
  ({ type: "string", required: true, description }) as const;

const commandArgs = {
  "validate-tag": {
    tag: stringArgument("Immutable Whisper runtime tag"),
    "github-output": {
      type: "string",
      description: "GitHub Actions output file",
    },
  },
  "assert-release-available": {
    repository: stringArgument("GitHub owner and repository"),
    tag: stringArgument("Immutable Whisper runtime tag"),
  },
  "qualify-archive": {
    target: {
      type: "enum",
      options: whisperTargetSchema.options,
      required: true,
      description: "Whisper runtime target",
    },
    archive: stringArgument("Canonical runtime archive"),
    "work-directory": stringArgument("Temporary extraction directory"),
    "team-id": { type: "string", description: "Expected Apple Team ID" },
  },
  "verify-published-release": {
    repository: stringArgument("GitHub owner and repository"),
    tag: stringArgument("Immutable Whisper runtime tag"),
    output: stringArgument("Temporary receipt path"),
  },
  "update-manifest": {
    manifest: stringArgument("Managed runtime manifest path"),
    receipt: stringArgument("Verified release receipt path"),
  },
  "verify-manifest": {
    manifest: stringArgument("Managed runtime manifest path"),
    receipt: stringArgument("Verified release receipt path"),
  },
  "manifest-status": {
    manifest: stringArgument("Managed runtime manifest path"),
    receipt: stringArgument("Verified release receipt path"),
    "github-output": stringArgument("GitHub Actions output file"),
  },
} satisfies Record<keyof typeof commandInputSchemas, ArgsDef>;

const validateTagCommand = defineCommand({
  meta: { name: "validate-tag", description: "Validate a Whisper runtime tag" },
  args: commandArgs["validate-tag"],
  async run({ args }) {
    const input = parseCittyInput(commandInputSchemas["validate-tag"], args);
    validateWhisperReleaseTag(input.tag);
    if (input.githubOutput) {
      await writeValidatedTagOutput(input.tag, input.githubOutput);
    }
  },
});

const assertReleaseAvailableCommand = defineCommand({
  meta: {
    name: "assert-release-available",
    description: "Require an unpublished Whisper runtime tag",
  },
  args: commandArgs["assert-release-available"],
  async run({ args }) {
    const input = parseCittyInput(
      commandInputSchemas["assert-release-available"],
      args,
    );
    await assertWhisperReleaseAvailable(input.repository, input.tag);
  },
});

const qualifyArchiveCommand = defineCommand({
  meta: { name: "qualify-archive", description: "Qualify a Whisper archive" },
  args: commandArgs["qualify-archive"],
  async run({ args }) {
    const input = parseCittyInput(commandInputSchemas["qualify-archive"], args);
    await qualifyWhisperArchive(
      input.target,
      input.archive,
      input.workDirectory,
      input.teamId,
    );
  },
});

const verifyPublishedReleaseCommand = defineCommand({
  meta: {
    name: "verify-published-release",
    description: "Verify a published Whisper release",
  },
  args: commandArgs["verify-published-release"],
  async run({ args }) {
    const input = parseCittyInput(
      commandInputSchemas["verify-published-release"],
      args,
    );
    await verifyPublishedWhisperRelease(
      input.repository,
      input.tag,
      input.output,
    );
  },
});

const updateManifestCommand = defineCommand({
  meta: {
    name: "update-manifest",
    description: "Update Whisper manifest entries",
  },
  args: commandArgs["update-manifest"],
  async run({ args }) {
    const input = parseCittyInput(commandInputSchemas["update-manifest"], args);
    await updateWhisperManifestFile(input.manifest, input.receipt);
  },
});

const verifyManifestCommand = defineCommand({
  meta: {
    name: "verify-manifest",
    description: "Verify Whisper manifest entries",
  },
  args: commandArgs["verify-manifest"],
  async run({ args }) {
    const input = parseCittyInput(commandInputSchemas["verify-manifest"], args);
    await verifyWhisperManifestFile(input.manifest, input.receipt);
  },
});

const manifestStatusCommand = defineCommand({
  meta: {
    name: "manifest-status",
    description: "Write managed Whisper manifest synchronization status",
  },
  args: commandArgs["manifest-status"],
  async run({ args }) {
    const input = parseCittyInput(commandInputSchemas["manifest-status"], args);
    const synchronized = await whisperManifestStatus(
      input.manifest,
      input.receipt,
    );
    await appendGithubOutput(input.githubOutput, [
      { name: "synchronized", value: String(synchronized) },
    ]);
  },
});

export const whisperReleaseCommand = defineCommand({
  meta: {
    name: "whisper-release",
    description: "Whisper runtime release automation commands",
  },
  subCommands: {
    "validate-tag": validateTagCommand,
    "assert-release-available": assertReleaseAvailableCommand,
    "qualify-archive": qualifyArchiveCommand,
    "verify-published-release": verifyPublishedReleaseCommand,
    "update-manifest": updateManifestCommand,
    "verify-manifest": verifyManifestCommand,
    "manifest-status": manifestStatusCommand,
  },
});

export async function runWhisperReleaseCli(argv: string[]): Promise<void> {
  await runCommand(whisperReleaseCommand, { rawArgs: argv });
}
