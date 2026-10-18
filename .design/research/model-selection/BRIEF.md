# Design research: model selection
Date: 2026-10-17
Mode: mixed
Status: researched

## Goal and constraints
Help a local AI user choose an appropriate model, install it, enable it, and select it for a conversation. Cover 320–1600 CSS px, both themes, existing React/CSS tokens and APIs. No backend changes or new dependencies.

## Evidence and limits
Inspected supplied mobile and desktop captures, current components and styles, LM Studio's rendered public download guide at 1280 and 390 px, Carbon's rendered filtering guide at 390 px, and Hugging Face/W3C page text. Public documentation is evidence of documented behavior, not direct testing of those apps. No competitor signed-in flows or user study. Capture dates of supplied images are unknown. Research accessed 2026-10-17 America/Chicago.

## References
R01: supplied desktop JPG ending 7839a676-2cc4-4a1d-8397-dc9f580aaddf.jpg and mobile PNG ending 9d5a7aed-f5a3-4664-b6fe-8bf4a6afa0ce.png in the supplied attachments. Actual app captures, viewport unknown. Observed clipped left edges in discovery and seven filter controls ahead of mobile results. Current source places long filenames in native selects and dense facts/actions in every card.

R02: https://lmstudio.ai/docs/app/basics/download-model . First-party guide, rendered and text inspected, publication unknown. Documents keyword/repository search and recommends 4-bit or higher when hardware supports it. Does not establish Q4 quality for every model. Mobile guide capture retained locally as browser-screenshot-lmstudio-ai-mvdc9a4o-746c4b20.png, outside the repository. Documentation page, 390×844 CSS px.

R03: https://www.carbondesignsystem.com/building-blocks/core/patterns/filtering . First-party design-system guidance, updated Aug 11 2026. Rendered narrow page and full text inspected. Describes instant and batch filtering, active counts on collapsed controls, and reset without reopening. Recommends separate categories rather than one multi-category menu.

R04: https://huggingface.co/docs/hub/gguf . Text-only inspection of metadata and quantization documentation, publication unknown. Observed separate file metadata and quantization types including 1/2-bit importance quantization. It does not promise a universally best quantization.

R05: https://www.w3.org/WAI/WCAG22/Understanding/reflow.html and https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/ . Text-only authoritative accessibility explanations, publication unknown. 320 CSS px reflow; named dialogs, keyboard containment, Escape and focus return.

## Decisions
D01: Search first, secondary filters including mode in an inline disclosure with active count and independent clear action. R01/R03. Keep instant local updates and URL persistence. Proposed mobile behavior: cards visible within the initial viewport. Test filter changes, collapse, clear, history restoration and empty recovery.
D02: Compact cards show name, status, download/memory and one primary install/enable action. Full capabilities, provenance and destructive/default actions live in named model details. R01, expert judgment. Preserve operation feedback and confirmations. Desktop uses two columns, mobile one.
D03: Recommend an available Q4_K_M or UD-Q4_K_XL with a comfortable fit, then a tight fit; never silently recommend IQ1/IQ2. Explain that Q4 balances size and quality, Q8 needs more memory, very low-bit files sacrifice quality. R02/R04 plus user preference. Unknown fit gets qualified language, not a machine-fit claim. All files stay reachable through a labeled advanced disclosure.
D04: Prefer a qualified existing catalog model when repository/name identity matches conservatively. R01/user requirement. Link directly to its details and existing install controls, avoid duplicate imports. Do not treat unqualified community entries as qualified.
D05: Picker separates usable and unavailable models, shows current selection and a clear management route. Proposed expert adaptation; no competitor picker interaction inspected. Preserve conversation and mode semantics.
D06: Use existing system type, muted surfaces and accent for primary actions. Long identifiers wrap, intrinsic grid/flex children can shrink, selects use constrained widths. Never fix overflow through page-wide clipping.

## Experience
Composer → choose ready model → return focus to composer. If unavailable → manage → search local catalog → inspect fit/details → install → enable → use by default when desired. Discovery → search → prefer qualified catalog match → otherwise inspect recommended download → optional alternate file → add and install or save without downloading. Preserve search on failures, announce success/errors, retry preview, preserve token/terms recovery, disable mutations for read-only access and during operations. No installation required for layout verification.

## Accessibility acceptance criteria
- Reflow, WCAG 1.4.10/R05: no sideways scrolling or lost actions at 320, 390, 768, 1280, 1600 px; measure document and open dialog scrollWidth and inspect long file names.
- Keyboard/dialogs, APG/R05 and WCAG 2.1.1/2.4.7: native disclosure/select/buttons, visible focus, Escape closes top dialog and restores opener; exercise keyboard traversal and focus return.
- Names/feedback, WCAG 3.3.2/4.1.2/4.1.3: visible field labels, explicit file radio names, polite result/operation status and alert errors; inspect accessibility tree and error fixtures.
- Touch: 44×44 CSS px goal for controls, stronger than WCAG 2.5.8's 24×24 AA minimum with exceptions; measure narrow layout.
- Contrast, WCAG 1.4.3/1.4.11: retain theme tokens, calculate relevant text/background combinations and inspect both themes. Full screen reader and representative user testing remain outside this desk review.
- Text enlargement: inspect 200% text and long content; preserve controls and reading order. No required motion.

## Avoid
A wall of filters, filename-only choice with no recommendation, automatic low-bit downloads, hidden offscreen content, and duplicated built-in models.

## Implementation handoff
src/ui/model-management.tsx, model-discovery.tsx, model-details.tsx, app.tsx picker region only, style.css; focused pure choice/matching tests and existing server-rendered component test patterns. Browser captures before/after, all widths/themes, bun run check and bun test ./src/ui. No production requests. No captured third-party images shipped or committed.
