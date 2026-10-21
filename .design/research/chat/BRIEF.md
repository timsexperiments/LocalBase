# Design research: Chat, settings, admin and discovery pages (Go M3)
Date: 2026-10-20
Mode: mixed
Status: partially researched

## Goal and constraints
The Go UI needs the pages TS has and Go lacks, Chat first. The user relies on Chat from a phone over the public hostname, so Chat must be ready before any cutover discussion.

Main task: hold a conversation with a local chat model, then come back to it later on the same device.

Order: Chat, settings, admin, a discovery page over `/_localbase/model-discovery`, then a phone review.

Constraints:
- **Stack:** server-rendered Go `html/template`, htmx 2.0.10 and the Tailwind standalone CLI, with no new dependencies. There is no React. Small hand-written modules under `static/js` are allowed, as `audio.js` already is.
- **Design:** the Shelf direction (`design/model-selection-v3`, `.design/research/model-selection`) and the existing tokens: canvas, line, ink, muted, accent, accent-soft, hover, on-accent.
- **Copy:** generic copy names no models.
- **Widths:** 320–1600 CSS px in both themes.
- **Content:** the TS UI is the functional spec (`src/ui/app.tsx` and its helpers).

## Evidence and limits
- **Capabilities:** web search, page text, and a local browser that could render public pages and capture them at 390×844. No sign-ins or form submissions were made.
- **The TS Chat:** could not be rendered. Its UI only serves after browser sign-in, which needs TLS email delivery and an HTTPS origin. The scratch gateway (127.0.0.1:24781) answered `ui_access_denied` and was stopped. The TS Chat evidence is therefore source-only (R01).
- **Earlier captures:** the TS model picker at 390 px exists in `.design/research/model-selection/screenshots`, but the conversation itself was never captured.
- **HuggingChat:** after its welcome dialog it requires sign-in, so only the empty state's DOM boxes and one capture behind the dialog are evidence.
- **chat.vercel.ai:** returned 404.
- **Untested:** no user study, and no screen-reader testing.

## References
**R01: the TS LocalBase Chat source** (`src/ui/app.tsx`, `client.ts`, `tools.ts`, `compaction.ts`, `steering.ts`, `reasoning.ts`, `attachments.ts`, `dictation.tsx`, `generation-settings.tsx`, `message-markdown.tsx`). This is the current product and the spec. Source-only; the build was not rendered. Observed in source:
- a drawer labelled "Open menu and history" with conversation search;
- "New conversation", and a model choice per conversation ("Choose model: …");
- a mode picker for chat, image, speech, transcribe, video, embeddings and decision;
- attachments ("Attach files", with chips that can be removed);
- "Stop generation" and "Send now (interrupts reply)", which steers a reply mid-stream;
- "Jump to latest message" and "Generation controls";
- an account menu, Settings and "Manage models";
- reasoning blocks and compaction dividers.

History stays on the device: `localStorage["localbase.playground.history.v1"]`, at most 30 conversations, without media or credentials, with reasoning capped at 20,000 characters. It can be turned off and cleared. Chat models with tool calling can call `generate_image`, `generate_video` and `synthesize_speech`, limited by permission. Markdown uses react-markdown with GFM.

**R02: HuggingChat** (https://huggingface.co/chat/), an open-source chat UI. Rendered at 390×844 on 2026-10-20; publication date unknown. The capture `screenshots/r02-huggingchat-390-welcome.png` shows the welcome dialog over the empty state. Observed from the page's accessibility tree:
- the header has an "Open menu" button (48×48) on the left, and "Share" and "New chat" on the right;
- the conversation list is an off-canvas drawer (332 px wide, placed at x=−331);
- the messages region is named "Conversation messages";
- the composer sits at the bottom: a 44 px tall input, with "Add attachment", "Start voice recording" and "Send message" at 32×32;
- the current model is a small link under the composer;
- suggestion chips sit in a horizontally scrolling row above the composer.

The signed-in conversation was not inspected.

**R03: "Accessible streaming chat UI"** (https://azukiazusa.dev/en/blog/accessible-streaming-chat-ui). Text only, accessed 2026-10-20; publication date unknown. Its recommendations:
- the history uses `role="log"` with a name and `tabindex="0"`, and `aria-live="off"`, so streamed partial text is never announced;
- a status element (`role="status"`, polite, atomic) is present from page load and announces start, completion, cancellation and failure;
- every message names its sender;
- focus stays in the input after sending;
- "Stop" is a real button, and focus returns to the input when it is pressed.

**R04: MDN.**
- `log` role (https://developer.mozilla.org/en-US/docs/Web/Accessibility/ARIA/Reference/Roles/log_role): the role implies `aria-live="polite"`, and chat logs are a listed use.
- viewport `interactive-widget` (https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Elements/meta/name/viewport): the default `resizes-visual` leaves the layout alone when the keyboard opens, while `resizes-content` resizes the viewport, and with it the viewport units. MDN's browser support table was empty.

Both text only, accessed 2026-10-20.

**R05: iOS focus zoom.** Text only, secondary sources: West Wind weblog (https://weblog.west-wind.com/posts/2023/Apr/17/Preventing-iOS-Textbox-Auto-Zooming-and-ViewPort-Sizing) and CSS-Tricks (https://css-tricks.com/?p=339455). Safari on iOS zooms into a focused input or textarea whose computed font size is under 16 px, and does not zoom back out. Disabling user scaling to avoid this harms accessibility. Not freshly verified against Apple documentation.

**R06: the earlier Shelf research** (`.design/research/model-selection/BRIEF.md`, D01–D06): search-first discovery, compact cards, a recommended quant, and a picker that separates ready models from unavailable ones. It applies unchanged to the discovery page and the model picker.

## Decisions
**D01: Chat is home, and on phones it gets a compact header instead of the tab grid.**
- Evidence: R01 and R02 both use a menu button that opens a drawer, plus a new-chat action. The current Go header spends about 100 px on a 3×2 tab grid.
- On Chat: below 1024 px the header holds the menu, the conversation title and model, and "New chat". The drawer holds conversation search and history, then the sections (Library, Get models, Running now, Lab, My voices, Settings, and Admin when permitted). At 1024 px and wider the drawer is a persistent sidebar.
- Other pages keep their tabs, now led by "Chat"; Settings and Admin are added to the nav, with Admin only for people who have its permissions. `/app` opens Chat.
- Test: at 390×844 the composer and at least three message lines are visible with no tabs. Every section is reachable from the drawer by keyboard. Escape closes the drawer and returns focus to the menu button.

**D02: Conversation layout.**
- Evidence: R01 (prose replies), R03 (sender naming).
- User turns are right-aligned bubbles of up to 85 % width. Assistant turns run full width as prose. Each turn has a visually hidden heading, "You" or "Reply".
- Long code blocks scroll inside themselves and never widen the page. Each code block and reply has Copy.
- Reasoning is a closed `<details>` ("Reasoning"), as in R01. An interrupted reply is marked "Interrupted".
- Test: no horizontal page scroll at 320 px with a 200-column code block.

**D03: A sticky composer that stays above the keyboard.**
- Evidence: R02 (bottom composer), R04, R05.
- The layout is a `100dvh` flex column with the message log as the only scroller. `interactive-widget=resizes-content` goes on the Chat page only. The textarea uses `font-size: max(16px, 1rem)` and grows to at most 40 % of the viewport.
- The send button is a 44×44 target (R02's 32 px is below that goal). While a reply streams, it becomes "Stop generation".
- Typing during a stream offers "Send now (interrupts reply)", as R01 does.
- Enter sends on devices with a precise pointer. On touch devices, Enter inserts a newline and the button sends; this is proposed, since TS's behaviour needs checking in source.
- Test: on an iOS simulator and an Android emulator, focusing the composer causes no page zoom and keeps the composer visible. This happens in the phone review.

**D04: Model choice.**
- Evidence: R01, and R06 D05.
- A header button shows the conversation's model and opens a sheet listing ready chat models first, with a link to Library for models that aren't ready. Other modes stay in Lab (Go M4), so Chat is chat only. The conversation keeps its model.
- Test: the picker opens, focus moves into it, Escape returns focus to the button, and the selection persists in the history entry.

**D05: History stays on the device, in TS's format.**
- Evidence: R01.
- Same key (`localbase.playground.history.v1`), the same schema and the same limits. History is not stored on the server. Settings has a toggle to keep history and a "Clear history" action.
- Cutover is same-origin, so conversations made in the TS UI open in the Go UI.
- Test: a fixture written by TS's `writeHistory` loads in the Go page, and the page writes JSON that TS's `historySchema` parses.

**D06: Streaming, rendering and announcements.**
- Evidence: R03, R04.
- The page calls `/v1/chat/completions` with `stream: true` as the signed-in person, the same session path the Lab uses.
- Markdown goes through a small DOM-building renderer that never uses `innerHTML`: paragraphs, headings, lists, fenced and inline code, emphasis, links (`rel="noopener noreferrer"`, http/https only), blockquotes and GFM tables. It is tested against R01's renderer on a fixture set.
- The history is `role="log"` with `aria-live="off"`. One status region, present from page load, announces "Reply started", "Reply finished", "Stopped" and errors.
- Auto-scroll only while the reader is already at the bottom. Otherwise show "Jump to latest message", as R01 does.
- Test: during a stream the status changes only at start and end, and scrolling up stops auto-scroll.

**D07: Recovery.**
- Evidence: R01.
- On failure, the draft and any attachments stay, the error is shown inline with Retry, and the turn is kept.
- With no ready chat model, the empty state says so and links to Library.
- Without `inference:chat`, the page explains that this account cannot chat.
- When the session expires, the existing `session.js` notice applies.
- Test: one fixture per state.

**D08: Settings** is the person's own settings: who is signed in and sign out, chat history (keep or clear), generation defaults (the R01 controls), and the Hugging Face token for people with `models:manage`. Admin is separate.

**D09: Admin** covers keys, browser users and roles, and access, ported from R01 `auth-management.tsx`. It uses Shelf list rows, named confirmations for destructive actions, and htmx partial updates. It gets its own focused review before it is built, because it carries the permission semantics.

**D10: Discovery** applies R06 D01–D04 unchanged to the Go endpoint.

## Experience
1. Open Chat: the last conversation is restored.
2. Optionally pick a model, type, and send.
3. The reply streams: Stop, or send-now steering, is available.
4. The reply finishes; copy it or continue.
5. Open the drawer, search history, switch conversation, or start a new one.

Each state has a defined form: empty, no model ready, model loading (the queue and model load show as "Starting the model…"), streaming, stopped, error, permission denied, session expired, and history turned off.

Untested hypothesis: on touch devices, Enter should insert a newline. Check this with the user in the phone review.

## Accessibility acceptance criteria
- **Reflow** (WCAG 1.4.10): no horizontal scroll at 320 px. Measure `scrollWidth` with long code blocks and long URLs.
- **Keyboard** (2.1.1, 2.4.3, 2.4.7): the drawer, picker sheet, composer, Stop and Copy all work by keyboard with visible focus. Escape closes the top layer and focus returns to its opener.
- **Names and roles** (4.1.2): the log has a name, each turn has a sender heading, and buttons have names. Inspect the accessibility tree.
- **Status messages** (4.1.3): one polite status region, with streamed text excluded (R03). A screen-reader check (VoiceOver) is required before cutover, since none was done here.
- **Targets:** 44×44 px for composer controls and drawer rows, which exceeds the 2.5.8 AA minimum of 24×24.
- **Zoom:** no `maximum-scale` or `user-scalable=no` (R05). Text inputs are at least 16 px.
- **Contrast** (1.4.3, 1.4.11): existing tokens, checked in both themes.
- **Motion:** no animation is required. Respect `prefers-reduced-motion` for scrolling.

## Avoid
- The 3×2 tab grid on the Chat page.
- Announcing streamed tokens.
- `innerHTML` for model output.
- Any server-side chat history.
- Model names in generic copy.
- Disabling zoom.
- Mixing other modes into Chat; they belong to Lab.

## Implementation handoff
New files:
- `go/internal/web/templates/chat.html`, `settings.html`, `admin*.html`, `discover.html`;
- `go/internal/web/chat.go`, `settings.go`, `admin.go`, `discover.go`;
- `go/internal/web/static/js/chat.js` (streaming, history and drawer) and `static/js/markdown.js`.

Changed: `layout.html` (Chat first, plus Settings and Admin).

Chat steps:
1. The core: page, drawer, picker, composer, streaming, markdown, history, all states.
2. The extras: attachments, dictation, steering, compaction, generation settings, and generation tools.

Tests:
- Go template and handler tests;
- a markdown fixture comparison against R01's output, generated as a TS golden;
- history compatibility fixtures, also TS goldens;
- browser checks at 320, 390, 768, 1280 and 1600 px in both themes;
- an iOS simulator for the keyboard.

Third-party captures stay local and are not committed.
