# What was checked

2026-10-18. Artifact scope only. No production runtime, LLM, GPU workload, launch service or live gateway was used. Catalog JSON was read directly; only public Hugging Face metadata was fetched. The only local server started for this task bound 127.0.0.1:24082. No new packages were added.

## Evidence

92 original PNGs are in screenshots. The required matrix is 3 concepts × 6 states × 2 widths × 2 themes = 72 images. States are picker, library, add with file choice, download in progress, memory warning and empty library. Another 20 images show Shelf's running state, Compass's task/priority/recommendation views, and Control room's running workspace. See screenshots/INDEX.md and manifest.json.

Capture viewports: 390 × 844 and 1280 × 900 CSS pixels. T3 used a device pixel ratio of 2 for the phone viewport; `sips --resampleWidth 390` normalized its PNGs to 390 × 844 image pixels. Desktop images are 1280 × 900. No fabricated reference screenshots or generated UI images are included. Browser screenshots show viewport crops; scroll and disclosures in the clickable files expose the remaining content.

Every capture was audited for rendered content, horizontal overflow, current theme, runtime error events and remote resource requests. None had overflow or runtime errors. Prototypes use inline data/CSS/JS; no external fonts, assets, gateway calls or backend requests occur. New captures were visually sampled across all three concepts and both themes, including the quick picker, file chooser, library, task launchpad and runtime workspace. This is visual review, not an independent usability study.

## Interaction checks

Actual browser clicks and keyboard input covered picker selection, edited-draft retention, native modal open/close, Tab and Shift+Tab wrap, Escape and focus restoration. DOM-driven scenario actions exercised simulated failures and terminal transfer states. The data log retains initial failures and successful corrective reruns, rather than erasing the history.

- The first picker Shift+Tab test escaped dialog focus. Added explicit first/last control wrapping and reran both directions successfully, including Control room's inspector. Escape closes and restores focus to the chat trigger.
- A global browser property collision initially prevented rendering. Scoped prototype code inside a closure. All capture and later reflow error arrays are empty. A missing running-page dispatch was caught on first visual review and fixed before the final capture set.
- Initial case-sensitive tests for pause and enable read CSS-transformed uppercase innerText. The underlying states were correct. Reran using textContent and both checks passed. These test-observation failures remain in verification-results.json with their corrective reruns.
- Qualified Qwen3.8 selection shows 17.77 GB from the pinned 17,772,537,440-byte artifact. Expanding both disclosures exposes 26 standalone weights; projectors and imatrix cannot be picked as a model. The two bf16 shards are listed together and unavailable in this qualified profile.
- Choosing an alternate file changes its warning. A simulated alternate download cannot enable before compatibility validation. Retry retains model and file. Verification precedes installed/disabled; explicit enable follows. Real pause/resume/cancel support still needs a server contract.
- Busy Clef cannot be unloaded. Unloading idle Qwen3.5 changes illustrative available memory from 16.0 to 25.2 GiB and retains its files. Current API defaults cannot be disabled or removed before replacement.
- Compass's Small footprint priority recommends Qwen3.5 4B rather than Qwen3.8 27B. Its inline picker returns to the draft. Other priorities are prompts for comparison, not measured quality rankings.
- Unknown memory produces a warning. Discovery failure offers retry and returns results. Access expiration and sign-in simulation reconnect to the existing local transfer selection. Empty-library acquisition starts at finding a model.

## Accessibility checks actually run

42 additional 320 CSS px checks: all three concepts, seven key states, both themes. No page-wide horizontal overflow or runtime errors; visible buttons met the 44 × 44 CSS px LocalBase touch goal. This is stronger than the 24 × 24 AA minimum where that criterion applies. Radio choices use full-size labels.

Six text-enlargement checks at 1280 px: Library and File choice for each concept, with each element's computed text size doubled. No horizontal page overflow. This is a controlled text-enlargement simulation, not Safari's browser zoom or dynamic type.

Fourteen contrast pairs were computed from source tokens, including ordinary/muted copy, selected text, primary buttons, warning text and control outlines. All tested text pairs exceed 4.5:1 and tested control outlines exceed 3:1. Examples: light muted-on-soft 4.77:1, light primary button 6.92:1, dark muted-on-soft 5.81:1, dark primary button 8.88:1. Computation covers opaque source pairs, not every composite background, hover or disabled state.

Native controls, named dialog, labeled inputs/radio groups and named progress were source inspected and sampled in T3's semantic snapshots. Reduced-motion CSS exists, and the prototypes introduce no essential animation. Announcements are designed with status regions and a named progress element, but VoiceOver output was not heard or verified.

## Not verified and remaining limits

No claim of WCAG conformance. VoiceOver, physical iPhone/Safari, real browser zoom, focus visibility on every scroll position, full keyboard completion of every cleanup path, all status announcements, Access sign-in, actual network suspension, disk exhaustion, checksum failure, concurrent API admission and restart durability remain untested. The prototypes simulate state in memory and reset on reload. They neither download weights nor reserve memory. They do not replace backend validation.

The resource fixture treats 64 GiB total, 28 GiB for other apps, and 20 GiB for loaded models as an illustrative budget. File sizes use decimal GB from manifests; peak memory uses GiB and an explicit request shape. No timing, quality, recommendation preference or frequency claim is presented as measured user evidence.

Build verification uses installed Bun and Node to regenerate all standalone files and syntax-check the prototype scripts. Artifact checks confirm all required screenshots exist, image dimensions match their filenames, and no production files enter the commit. Application tests were not run because no production source, contracts or dependencies changed.

The self-contained REVIEW.html comparison was checked with T3 html_preview at 728 px/light and 390 px/dark. Its images are explicitly reduced JPEG thumbnails of the original PNGs to keep the inline document under the host's size limit. Original evidence remains in screenshots. The first inline attempt exceeded the host's 512 KB HTML limit; only the thumbnail representation changed.
