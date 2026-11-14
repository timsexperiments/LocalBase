// Where the runtime release scripts find what lives outside scripts/runtimes. One constant each, so
// a move changes one line here.
import { resolve } from "node:path";

// The managed runtime manifest: the Go gateway embeds it, and the sync workflows update it.
export const repositoryManifestPath = resolve(
  import.meta.dir,
  "../../../go/internal/runtime/managed-runtime-manifest.json",
);
