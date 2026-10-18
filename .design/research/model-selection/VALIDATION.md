# Model selection validation

## Review round 2, 2026-10-18

Rebase onto latest origin/main was a no-op: the PR already includes #364 at 34eb898. The session-notice behavior remains in place; app.tsx changes are limited to the picker and the drawer's optional header action.

### Changes against the five findings

1. File safety: source inspection confirmed the backend already expands shard sets and totals every downloaded file, but only excluded mmproj from language-model choices. The shared classifier now excludes projectors, MTP/draft heads, importance matrices, adapters/LoRAs and tokenizer/vocabulary GGUFs. Explicit adds reject those targets too. Vision models still pair with their projector. Discovery exposes partCount and shows one BF16 option with total size and “2 parts, total”. The suggestion is first; alternatives follow increasing total size rather than pathname.
2. Density: cards use name/state, facts and actions. The uninstalled badge is removed. Matching chat quant variants share one card with a native variant select; facts, install/enable controls and details follow that selection. At 390px the initial cards are 125–127px tall, with four full model cards visible below the unchanged 321px first-result position. At 320px the first cards measure 125–147px. Quant grouping reduces the fixture's 76 entries to 72 model cards; the count reports both models and variants.
3. Picker: Manage models is in the header beside the title. Search, helper text, left-aligned mode buttons and ready models follow without the former standalone link gap. Mode buttons and the management link have 44px targets.
4. Selection clarity: unknown quantization reads “Selected download · Quantization unknown” and is never recommended. Filename, download total, memory, quant and fit are inside the selected-download panel. Verified Q4 → Q8 → BF16 changes update every fact and the selected-file label.
5. Reflow: 80 measured width/theme/state combinations had document and open-dialog scrollWidth equal to clientWidth. Widths were 320, 390, 768, 1280 and 1600px, both light and dark. States were catalog, expanded filters, details, lab picker, expanded picker, chat picker, expanded discovery files and selected BF16 shards. No overflowing content children were found. Simulated 200% text sizing of discovery at 320px also had no horizontal overflow. Changed catalog buttons/selects, mode buttons and Manage models link measured at least 44px high.

### Round-2 evidence and limits

Actual rendered app captures from T3 Chromium on loopback port 26752. The preview serves the current app bundle with the built-in catalog through the real metadata projection, simulated installed/ready state and a fake authenticated local session. Discovery fixtures run through previewRepository with a fixture Hub response containing MTP, mmproj, draft, imatrix and two BF16 shards. The unknown-quant result is an explicit UI fixture. No publisher search or actual model installation was performed in this round. Fixture sizes are illustrative. No inference, production requests or model-directory writes occurred.

Exercised variant selection, matching details navigation, Escape/focus return, picker-to-management navigation, file radios and selection summaries. API/CLI classification tests cover auxiliary rejection and shard totals. Screen-reader and physical iPhone/Safari verification remain unperformed. The text-size check is a simulation, not a browser-zoom certification.

### Round-2 before and after captures

The previous round's after captures are this round's before evidence; they are preserved without edits:

- [Before: mobile catalog, dark](screenshots/after-catalog-mobile-dark.png)
- [Before: mobile picker, dark](screenshots/after-picker-mobile-dark.png)
- [Before: mobile file chooser, dark](screenshots/after-file-choices-mobile-dark.png)
- [Before: desktop discovery, dark](screenshots/after-discovery-desktop-dark.png)

New captures use 390×900 and 1280×900 CSS viewports. Mobile PNGs have a 2× device scale. Discovery captures are scrolled to the relevant result/selection panel; they are full viewport captures, not crops.

- [After: mobile catalog, dark](screenshots/round2-catalog-390-dark.png), [light](screenshots/round2-catalog-390-light.png)
- [After: desktop catalog, dark](screenshots/round2-catalog-1280-dark.png), [light](screenshots/round2-catalog-1280-light.png)
- [After: mobile lab picker, dark](screenshots/round2-picker-390-dark.png), [light](screenshots/round2-picker-390-light.png)
- [After: desktop lab picker, dark](screenshots/round2-picker-1280-dark.png), [light](screenshots/round2-picker-1280-light.png)
- [After: mobile file choices, dark](screenshots/round2-file-choices-390-dark.png), [light](screenshots/round2-file-choices-390-light.png)
- [After: desktop known/unknown quantizations, dark](screenshots/round2-discovery-1280-dark.png), [light](screenshots/round2-discovery-1280-light.png)
- [After: selected BF16 shards with total size](screenshots/round2-sharded-selection-390-dark.png)

### Round-2 automated checks

- bun run check: passed, including formatting, TypeScript and CLI help.
- bun test ./src/ui: 201 passed, 0 failed, 22 files.
- bun test ./src/domains/models/huggingface.test.ts ./src/domains/models/discovery.test.ts: 43 passed, 0 failed.
- git diff --check: passed.

The remaining sections preserve round-1 evidence and checks.

## Round-1 validation

Verified on 2026-10-17 using the T3 collaborative Chromium preview against an isolated scratch gateway on loopback ports 26741/26742. Catalog and Hugging Face search responses were real. Only the 42 MB Whisper Tiny Q8 model was installed, in the scratch root. No inference or production gateway requests were made.

## Before and after

At 390 CSS px the first catalog card starts around 321 px after the redesign, compared with 562 px before. Its install and details controls fit in the initial viewport. Secondary filters are a labeled disclosure with an active count, an independent reset, and a quick Installed toggle. Desktop retains two-column results and a three-column expanded filter grid.

Discovery replaces a filename-only native select with an explained Q4 suggestion, full wrapping filenames and a radio list behind “Choose another file”. IQ1/IQ2 choices remain available with a quality warning. The Qwen3.8-27B result leads with the qualified Local catalog entry and collapses its community download route.

The composer picker leads with installed, enabled models. Models requiring setup are in a separate disclosure; their labels explain whether to install or enable. Full model details retain provenance, capabilities, license, files, installed size and default/uninstall actions.

## Screenshots

These are actual local app captures, with a fake signed-in scratch session. Pixel dimensions can be twice the CSS viewport because of device scale. The before mobile capture is 390×844 CSS px; after mobile captures are 390×900, except the ready picker at 390×844. Desktop comparisons are 1280×900.

- [Before: mobile catalog, dark](screenshots/before-catalog-mobile-dark.png)
- [After: mobile catalog, dark](screenshots/after-catalog-mobile-dark.png)
- [After: mobile catalog, light](screenshots/after-catalog-mobile-light.png)
- [After: desktop catalog, dark](screenshots/after-catalog-desktop-dark.png)
- [Before: desktop discovery, dark](screenshots/before-discovery-desktop-dark.png)
- [After: desktop discovery, dark](screenshots/after-discovery-desktop-dark.png)
- [After: mobile discovery, dark](screenshots/after-discovery-mobile-dark.png)
- [After: mobile file choices, dark](screenshots/after-file-choices-mobile-dark.png)
- [After: mobile ready picker, dark](screenshots/after-picker-mobile-dark.png)

The supplied desktop clipping was not reproduced in this scratch browser before the change. The before capture does demonstrate filename truncation. The redesign removes that select and checks the wrapping file list directly.

## Rendered checks

- Picker, catalog, details and discovery inspected/captured at 320, 390, 768, 1280 and 1600 CSS px in light and dark. Document and every open dialog had scrollWidth equal to clientWidth at those sizes.
- Expanded file choices also measured without horizontal overflow at all five widths. Full filenames remain readable. Expanded filters use the entire content width, with 262 px selects at 320 px, and a full-width sort row with its reverse control.
- Visible catalog buttons, search and filter controls measured at least 44 CSS px high. File radios are 24 px within larger clickable labels. Active filter/reset buttons wrap to another row when space is limited.
- Simulated 200% text enlargement at 320 px, by doubling computed font sizes in the model-management subtree: no horizontal overflow; toolbar wraps. This is a text-sizing simulation, not an operating-system accessibility or browser zoom certification.
- Keyboard: visible focus, filter toggle, Tab traversal, native radio selection; Escape closes only details, returning focus to its opener. Discovery-to-details returns focus to catalog search when its original opener is unmounted. Choosing Whisper Tiny closes the picker and restores the composer picker button when opened there.
- Real scratch mutations: disabled and re-enabled Whisper Tiny, then set it as default. Opened uninstall confirmation without removing the model. Checked search empty recovery, filter reset, query-string updates, qualified catalog navigation and low-bit warning selection. Large chat models were never installed.
- Success, error, permission, pending and read-only branches remain in the existing API flow. Server-rendered component tests verify disabled read-only discovery actions; no screen reader or Safari/iOS runtime test was performed.

## Contrast

Calculated from the active opaque theme tokens, not inferred from screenshots. Ordinary text on surface: dark 12.23:1, light 15.02:1. Muted text on surface: dark 6.44:1, light 5.55:1. Muted text on canvas: dark 7.26:1, light 5.29:1. Primary action text: dark 8.88:1, light 6.92:1. Primary hover: dark 10.68:1, light 9.37:1. Accent on soft surface: dark 6.97:1, light 5.84:1. This is selected text contrast coverage, not a full conformance audit.

## Automated checks

- `bun run check`: passed, including Prettier and TypeScript.
- `bun test ./src/ui`: 197 passed, 0 failed, 22 files.
- New coverage: Q4 recommendations and hardware fit ordering, exclusion of low-bit defaults, unknown fit, exact qualified-family matching, derivative/version rejection, picker readiness/search/empty states, progressive discovery choices, qualified route precedence and read-only actions. Extended details coverage for installed disk size.

## Remaining validation

Test VoiceOver and Safari on a physical iPhone, and ask a new user to choose/install a model without knowing quantization names. The recommendation is a heuristic from catalog fit estimates, not a new model-quality or memory benchmark. Next redesign candidates are the composer attachment/tool flows and generation settings.
