# Model selection validation

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
