# Implementation after a direction is selected

No application code changes in this round. Recommended direction: Shelf. The plan uses the existing React/Bun stack and LocalBase's CSS tokens. Approximate phases below are sequencing, not estimates backed by engineering discovery.

## Keep and reshape

| Inspected code | Proposed treatment | Why |
| --- | --- | --- |
| src/ui/app.tsx, Drawer at line 303 and model-picker trigger at 1973 | Keep native dialog, draft state and focus restoration. Extract a RecentChatPicker that only reads enabled, installed chat-capable models. Give it a return-aware link to add a model | Protect the daily job and exclude decision/embedding-only models |
| src/ui/model-management.tsx, ModelManagement at 189 | Replace its primary Local catalog / Find more split with ModelsWorkspace routes for Library, Get models, Running now. Extract InstalledModelList, ModelResourceSummary and ModelActions | Ownership and runtime state become explicit destinations |
| src/ui/model-discovery.tsx, ModelDiscovery at 485 and DiscoveryCard at 183 | Keep discovery request handling and failure/token recovery. Replace the file select at 344 with ModelFamilyDetail and ArtifactChoice radios, qualification provenance and optional variant comparison | Compare files within a model, with one justified default |
| src/ui/model-details.tsx | Reuse license, capability and source detail; separate installed actions, enable/default actions and residency actions | Retain trust data without placing every property in inventory rows |
| src/ui/navigation.ts | Add durable models routes and return-to-conversation context. Preserve shareable model detail URLs and browser Back | Avoid a maze of stacked modal drawers |
| src/ui/client.ts and src/catalog/catalog-query.ts | Reuse parsed metadata, capability labels and catalog query logic. Add a projection for model-family grouping, source revision, file support and unknown/stale resource state | A family is not the same as a qualified install recipe |
| src/ui/style.css | Keep canvas/surface/text/accent theme tokens, focus styles and touch minima. Add responsive installed rows, file comparisons and resource layouts | Preserve product identity and existing stack |
| src/ui/hf-token.tsx and auth/access code | Keep permission and token flows. A user who can read models must not gain management controls | Existing authorization boundaries remain authoritative |

## Existing backend contracts to reuse

`/_localbase/model-management` already supplies storage availability, per-model installed/enabled/active state, byte counts, canInstall, unavailable reason and operation states running/complete/failed. Its action union is install/uninstall/enable/disable/activate. Read operation detail, not just the boolean "installed". Determine the current semantics of activate before mapping any UI verb to it; it must not accidentally combine conversation choice, API default and residency.

`/_localbase/model-discovery` is the existing discovery request boundary. Keep the server-side validation and qualified recipes. Existing model metadata supplies memory estimate sources, host memory and fit verdicts. Reuse those projections. Do not derive physical fit from disk bytes in the client.

## Required backend discovery and additions

1. Family and artifact identity: canonical repository/revision, qualified catalog identity, file sizes, support status per runtime profile, related projectors and shard grouping. Avoid deduplicating unlike runtimes solely because display names match. Recommendation must name its evidence and support unknown fit.
2. Durable operations: stable operation IDs and timestamps, download/verify/install stages, authoritative byte totals, reconnectable status, duplicate-request protection and server-owned cleanup. Pause, resume and cancellation in the prototypes are concepts, not current guarantees. Ship retry/status first if those actions lack safe semantics.
3. Separate mutations: explicit enable/disable and defaults by capability. Decision and embedding are both llm in catalog kind but cannot share a chat default. Preserve legacy endpoint behavior during migration and validate the capability on the server.
4. Runtime inspection: model identity, busy/idle, request owner in privacy-appropriate form, measured allocation versus estimate, provenance, last update time, and a safe unload operation that refuses active work. Never expose request content in the owner label.
5. Admission explanation: estimate at request shape plus reserve and current commitments, with fresh load-time admission. Memory unknown/stale must not receive a positive fit label. An explanatory plan is not a capacity lock. Use existing host/unified-memory accounting and measured profiles to avoid double-counting Metal/host memory.
6. Recovery: Access expiration preserves the operation reference and form choices. After reauthentication, fetch operation state before retry. Restore or reject mutation actions according to current permissions; do not bypass token/access checks.

## Phases and gates

Phase 1, daily and weekly jobs: extract picker, add installed-only Library and read-only Running now, preserve chat draft/selection and URL navigation. Gate: a recent-model switch takes two taps and changes no API default; Clef and embeddings never appear in the chat picker; 390 px and desktop both work.

Phase 2, acquire: merge source search, family detail, qualified file first, exact provenance and variants. Reuse existing install operations. Gate: actual Qwen3.8 pinned Q4_K_M bytes are 17,772,537,440, file revisions stay coherent, projectors/imatrix are not standalone model choices, shards install together, errors preserve the selected artifact. Image/speech recipes retain all required artifacts.

Phase 3, remote operations and management: stable transfer identity/reconnect, explicit enable/default/cleanup, supported queue actions and operation completion. Gate: browser closure, Access renewal and duplicate retry cannot produce conflicting installs; destructive removal shows impact and protects defaults.

Phase 4, capacity explanation: integrate authoritative busy/idle runtime data, unknown/stale state and load-time admission explanation. Add unload only after safe server behavior exists. Gate: no busy client request can be evicted through this screen, and accounting agrees with the runtime's unified-memory model.

Use existing src/ui/model-management.test.ts, model-management-access.test.ts, model-details.test.ts, navigation.test.ts and client.test.ts for changed behavior, plus real server contract tests for operation races and admission. Do not mirror markup with brittle tests. Run the repository's check/build gates when application changes are actually made. This artifacts-only round uses browser interactions, syntax checks and screenshot coverage instead.

## Open questions for the next engineering pass

Do all runtimes expose safe model-level unload, or only whole-runtime stop? Is "active" currently a default, resident, or both? Which install operations survive process restart, and which transfer actions truly support resume? What telemetry can distinguish a busy API owner without revealing private request data? These affect implementation scope, not the owner's ability to choose a concept now.
