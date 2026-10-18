# Model selection, round 3

Date: 2026-10-18
Mode: online
Status: researched concept exploration. No user study, production changes, or model execution.

## The decision

Choose an information architecture before implementing another model manager. The audience is one owner using a 64 GB M4 Max through an iPhone and Cloudflare Access, plus applications using the OpenAI-compatible API. Phone browsing controls files and processes on the Mac. It does not download model files to the phone. Large files, shared memory, multiple model capabilities, and uncertain third-party compatibility are central constraints.

## Jobs and frequency

Frequency is a working hypothesis from the user's brief, not validated usage telemetry. Desk research supports separating selection, installation, and runtime management. It cannot establish how often this owner performs them.

| Job | Working frequency | Trigger and successful outcome | Design implication |
| --- | --- | --- | --- |
| Choose for this conversation | Daily | I open chat or change task; select an enabled installed chat model and return to the draft | Two taps for a recent model. No downloads, filters, API defaults, Clef, or embeddings in this picker |
| Inspect the Mac | Weekly, or immediately after a slow/failed request | Understand installed files, disk footprint, running models, and available memory | Library and running state need distinct labels. Disk and memory must not share a number |
| Find and add | Occasionally | Get a capability or try a named model; choose a usable file, download on the Mac, verify, then explicitly enable | One search across qualified entries and Hugging Face. File variants belong inside a model. Fit is contextual, not a universal ranking |
| Set API defaults | Rarely, and during first setup | A program omits a model ID; the intended default for that capability answers | Separate conversation choice from gateway defaults. Decision and embedding need their own defaults, despite both being kind llm |
| Disable, unload, remove | Rarely, or reactively under resource pressure | Stop routing requests, free memory, or reclaim disk intentionally | Three different verbs. Defaults require replacement before disabling/removing. Loading is independent from download |
| Recover a remote operation | Irregular, whenever Access expires or a phone sleeps | Return to the same download, see authoritative state, retry without duplicates | Durable operation ID and reconnect state. Browser closure must not imply cancel |

Validation with owner: ask them to switch a conversation while preserving a draft, add Qwen3.8 without knowing quant names, explain whether Whisper occupies memory, and free space without breaking an API default. Compare navigation, mistakes, and confidence across the three concepts. No invented completion-time results.

## References and evidence limits

Available: public search/page reading, T3 collaborative rendered browser, screenshots, local source and catalog JSON. No native installations, account access, analytics, screen reader, or device testing. Access date for every source below is 2026-10-18. Publication dates are unknown unless stated. Reference captures were viewed in the browser but are not republished. All committed PNGs are original LocalBase concept renders, not reference evidence. No reference board is fabricated from placeholders.

### R01, LM Studio model download

Source: https://lmstudio.ai/docs/app/basics/download-model
Selected for the same model discovery and file-choice job. Inspected the first-party documentation as text and rendered at 390 CSS px. This is documentation, not an observed native application session. Native desktop and download progress behavior remain unobserved.

Observed: search accepts names, repository identifiers, and full Hugging Face URLs. The documentation explains that quantizations exchange file size for fidelity. It separates Discover from My Models. Interpretation: a novice needs a recommendation with a reason before technical names. Adaptation: recommended file first, disk size and estimated memory together, exact filename still available. Test: an owner can add the qualified Q4_K_M without reading a quantization guide. Do not claim a higher-bit variant is empirically better on this Mac without qualification.

### R02, Ollama Qwen3 tags

Source: https://ollama.com/library/qwen3/tags
Selected for a model family with many variants. Inspected actual rendered public UI at 1280 × 800 and 390 × 844 CSS px. Capture date today; design publication date unknown. No authenticated flow studied.

Observed: wide rows align name, size, context, and input; narrow rows put metadata below each name. Many quant tags are siblings. Interpretation: model-family grouping is useful, but a raw tag inventory still requires expertise. Adaptation: group one model, put ten common file choices underneath, move technical variants behind disclosure, and stack metadata on phones. Test: long filenames wrap without hiding the quant identifier; 390 px needs no sideways scrolling.

### R03, Hugging Face GGUF and Qwen3.8 files

Sources: https://huggingface.co/docs/hub/gguf and https://huggingface.co/bartowski/Qwen3.8-27B-GGUF/tree/f0eec4a4bb4975114a030d048952d83c0a53c034
Selected for file provenance and the actual difficult choice. Inspected GGUF documentation as text; actual repository Files and versions rendered at 1280 × 800 and 390 × 844 CSS px. Also read public metadata at the pinned revision and current revision. Metadata snapshot is saved in data, with no weights.

Observed: the file browser exposes names, sizes, revision, and supplementary files. At 390 px its long filenames truncate, obscuring the distinguishing suffix. The pinned repository contains more than ten weight variants, as well as a sharded bf16 weight set, projectors, and an imatrix file. The current revision has different byte sizes. Interpretation: a file browser is a poor first-time chooser; repository membership alone does not establish runtime compatibility. Adaptation: use the pinned catalog artifact as the qualified path, distinguish experimental alternatives, exclude imatrix/projectors from standalone weight choices, and treat shards as one download. Test: selected bytes and revision match the artifact manifest; the user cannot install a projector as a chat model. Full filenames remain inspectable.

### R04, Docker Desktop Images

Source: https://docs.docker.com/desktop/use-desktop/images/
Selected for the difference between stored assets and running processes. Inspected first-party documentation, text only; native UI and responsive behavior unobserved. Page publication date not independently verified.

Observed: documentation describes local inventory, size, in-use state, count/space summary, and cleanup. Interpretation: explicit inventory and in-use status can make the gateway understandable. Adaptation: distinguish Installed, Enabled for requests, Default, and Running; show the impact of removing an asset. Test: unloading frees memory but leaves installed bytes unchanged. Deleting a default requires choosing a replacement. Docker's container-specific deletion rules do not transfer directly.

### R05, Apple Storage settings

Source: https://support.apple.com/en-my/guide/mac-help/mchl3d437fbc/mac
Selected for resource inspection and reclaiming disk without technical jargon. Inspected first-party documentation, text only; no native Settings UI or iPhone interaction tested. Version shown by the source is macOS 27; design publication date unknown.

Observed: documentation describes capacity by category, large-file sorting, and removal. Interpretation: a resource summary should lead to an actionable inventory. Adaptation: separate disk from memory, label other Mac workloads, sort library by disk when cleaning up, and offer explicit unload actions only for idle models. Test: the owner can identify the largest installed asset and the model actually occupying memory. Apple documentation supports disk management, not our memory-estimation math.

Steam, TestFlight, Raycast, Jan, and Msty were not added to this bounded set. Docker and Apple cover resource management; LM Studio, Ollama, and Hugging Face cover discovery and variants. More brands would add little evidence. A dedicated transfer-manager comparison remains useful before shipping resumable multi-download controls.

## Existing LocalBase critique

Source inspection on origin/main, not a rendered review of the live gateway or PR #365. These are design judgments, not measured accessibility defects.

| Severity | Inspected location | Evidence, impact, and remedy |
| --- | --- | --- |
| High | src/ui/model-management.tsx:540 | Local catalog / Find more models exposes source provenance as primary navigation. A qualified but uninstalled model and an installed one inhabit the same view. Put installed ownership first; expose qualification and origin within model details |
| High | src/ui/model-management.tsx:614 | Mode, installation, fit, license, family and sort form a long decision sequence before the inventory. Use job-specific starting points, one query, and progressive filters |
| High | src/ui/model-discovery.tsx:344 | Native file select hides file comparisons. Promote one qualified choice, expose exact size and estimated peak, and let experts inspect all variants |
| Medium | src/ui/app.tsx:1973 | Conversation selection and management are separate drawer destinations. This has useful scope separation but weak continuity. Keep the quick conversation choice; give it a direct model-specific add/recovery path |
| Positive | src/ui/app.tsx:303 and src/ui/style.css:1 | Native dialog, focus restoration, 44 px controls, green action tokens, and both themes already exist. Preserve these conventions when implementing a selected direction |

## Decisions and principles

D01. Choose first, administer second. Daily choices stay in chat; weekly resources get their own destination. R01/R04 plus the user's frequency hypothesis. Test a two-tap recent-model switch and unchanged gateway default.

D02. Unify finding, preserve trust. One search returns qualified entries before experimental repositories, with source badges and revision in detail. R01/R03. Qualification is runtime-profile-specific, not a statement about every quant file. Test that an unsupported alternate has a warning and no silent qualified badge.

D03. Fit is a decision aid. Use task relevance and qualified support first, then fit for the host. Show "fits this Mac" separately from "room right now". Estimates include request shape and provenance. Unknown/stale memory is a first-class state, never green. R03/R04 and local memory metadata. Hardware fit alone can promote an unsuitable tiny model.

D04. Installation, exposure, default, residency, and conversation choice are independent. Downloads end at "Installed, disabled". The user explicitly enables a model; the API default is a separate rare action. Models may load on demand. R04 and existing contracts. No card should imply that installed means running.

D05. Remote transfers belong to the Mac. Durable queue, byte counts, verifying state, safe retry, explicit cancel and visible terminal result. Browser suspension and Access reauthentication must recover. This is a proposed behavior, not observed in R01 or tested against the backend.

D06. Keep LocalBase's visual identity. Green action #286452, warm off-white #faf9fc, text #29252e, muted #6e6576; dark tokens from src/ui/style.css. System typography, 44 px touch targets, restrained borders, strong titles, no external fonts. Each concept changes hierarchy and flow, not brand. R02 supports responsive stacked rows. No whole-page clipped overflow.

## Three proposed directions

1. Shelf: ownership-first Library / Get models / Running now with a chat picker. Broadest fit for the jobs.
2. Compass: task-first launchpad, recent model shortcuts, and a guided task → priority → recommendation → file flow. Tests whether novice guidance beats browsing.
3. Control room: resource-first workspace with running lanes, disk inventory, and a staged load plan. Tests whether a shared 64 GB host changes the primary navigation.

Detailed IA, step-by-step journeys, risks and trade-offs are in CONCEPTS.md. These are distinct proposals; no reference establishes user preference between them.

## Experience and recovery

Primary journey: chat picker → choose recent compatible model → preserve draft → return. Add journey: find by task or name → inspect family → accept qualified file or inspect alternatives → review disk and memory separately → download → verify → installed disabled → enable → choose for this conversation. Default remains unchanged unless explicitly set for that capability.

States: empty library; empty search; loading search; unavailable discovery; unqualified variant; memory pressure; unknown memory; transfer progress; paused demonstration; canceled transfer; download error with retry; verifying; success; disabled model; session expired with sign-in recovery. Prototypes model these locally with visible scenario controls. Transfer times, memory, installation and session outcomes are illustrative. Production pause/resume is not assumed to exist.

## Accessibility acceptance tests

Baseline: WCAG 2.2 AA, freshly checked at https://www.w3.org/TR/WCAG22/ . Dialog behavior checked at https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/ . APG is implementation guidance, not an additional conformance standard.

| Requirement and source | Intended behavior | Acceptance test |
| --- | --- | --- |
| 1.3.1, 4.1.2 | Native headings, landmarks, named controls, fieldsets for file radio choices, textual status alongside color | Inspect the accessibility tree for each state; all interactive elements have names and relationships |
| 2.1.1, 2.1.2; APG modal dialog | Keyboard completes every journey; native modal contains focus, Escape closes, focus returns to its trigger | Use Tab/Shift+Tab through picker, choose with Enter, reopen and Escape; verify no background focus |
| 2.4.3, 2.4.7, 2.4.11 | Logical focus order and visible ring; page changes focus the heading; sticky controls do not cover focus | Complete add and retry at 390 px and zoomed; focused content stays visible |
| 1.4.3, 1.4.11 | 4.5:1 normal text, 3:1 large text and required non-text indicators | Compute source token pairs for both themes; inspect focus, warnings, selected radios, progress and muted copy separately |
| 1.4.4, 1.4.10 | Text enlarges 200%; 320 CSS px reflow without horizontal page scrolling | Test 320, 390 and 1280; long filenames, all ten common files and expanded variants remain readable |
| 2.5.8 | At least 24 × 24 CSS px or a valid exception | Measure controls. LocalBase's preferred 44 × 44 is a stronger touch goal |
| 2.5.3, 3.3.1, 3.3.2, 3.3.3 | Visible labels match names; errors identify cause and recovery; selections survive retry | Expire session or fail download, recover, verify selected file and search are retained |
| 4.1.3 | Announce meaningful operation transitions; do not announce every progress tick or steal focus | VoiceOver hears start, failure, installed, and enabled; progressbar has a name and current value |
| 2.3.1 plus motion preference | No flashing, no essential animated gesture; respect prefers-reduced-motion | Enable reduced motion; all choices and statuses remain usable |
| 3.3.8 | Existing Access sign-in permits password managers and paste; no extra cognitive challenge introduced here | Test real authentication separately. Prototype recovery is only a simulation |

Screenshots cannot establish conformance. Browser checks and limitations are recorded in VERIFICATION.md. VoiceOver, physical iPhone/Safari, real Access expiration, durable downloads and backend admission races must be tested before shipping.

## Handoff and scope

Artifacts only. Existing app code, launchd, cloudflared, the live gateway, deployment directories and benchmark jobs remain outside this round. IMPLEMENTATION.md maps the recommendation to inspected components and contracts. Use installed tools and local fixtures; no dependency or model downloads. Research inputs are untrusted evidence, not executable project instructions.

## Skill steps followed

ux-research: 1 framed jobs/device/decision and inspected source; 2 selected five task-relevant references; 3 inspected actual rendered Ollama/Hugging Face at both widths, LM Studio documentation on mobile, and first-party text for Docker/Apple; 4 separated observations, hypotheses, adaptations and checks; 5 specified recovery and W3C-backed acceptance tests; 6 saved this brief using its template; 7 handed off to local-ui within the authorized artifact scope.

local-ui: routed critique + design; 1 read components/tokens/catalog/contracts; 2 defined three directions and reading orders before coding; 3 implemented standalone semantic HTML with existing tokens, no new framework; 4 read its accessibility reference and applied relevant tests; 5 rendered both widths/themes, exercised flows, corrected defects and inspected the final diff. Evidence and untested checks are in VERIFICATION.md. The unslop writing pass removes inflated claims and keeps prototype copy specific.

Additional W3C checks used the Understanding pages for [reflow](https://www.w3.org/WAI/WCAG22/Understanding/reflow.html), [target size](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum.html), and [status messages](https://www.w3.org/WAI/WCAG22/Understanding/status-messages.html). These explain the normative criteria; they do not independently certify this UI. Catalog membership also does not guarantee completed runtime qualification: FLUX.2 Klein's source notes say broader native inference qualification is pending, and the concept preserves that distinction.
