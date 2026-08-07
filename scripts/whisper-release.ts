import { runMain } from "citty";
import {
  runWhisperReleaseCli,
  whisperReleaseCommand,
} from "./whisper-release/cli";

export {
  qualifyWhisperArchive,
  type CommandRunner,
} from "./whisper-release/archive";
export {
  validateWhisperReleaseTag,
  whisperReleaseReceiptSchema,
  type Fetcher,
  type WhisperReleaseReceipt,
} from "./whisper-release/contracts";
export {
  updateWhisperManifest,
  verifyWhisperManifest,
  whisperManifestMatches,
} from "./whisper-release/manifest";
export {
  assertWhisperReleaseAvailable,
  verifyPublishedWhisperRelease,
} from "./whisper-release/published-release";
export { runWhisperReleaseCli, whisperReleaseCommand };

if (import.meta.main) {
  await runMain(whisperReleaseCommand);
}
