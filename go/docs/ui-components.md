# UI components

The inventory for UI v2 (DESIGN §25). Styles are in `internal/web/css/input.css`, values in `internal/web/css/theme.css`, shared templates in `templates/layout.html` and `templates/parts.html`.

"v2" says where a component stands: **done** uses Hearth by design, **tokens** has only picked up the theme so far, and its reworked page will change it further.

## Identity

| Component | Where | v2 |
|---|---|---|
| Mark (`mark`) | `layout.html`; files in `static/brand/` | done |
| Memory strip (`memory-strip`, `.strip`) | Studio, Models, Get models; Chat and first-run next | done |
| Memory gauge (`gauge`, `memory-legend`) | Strip, Running | done |

**Mark usage:** always this shape, in one color or with its lamp; never on a busy picture, and at 16 px or larger.

## Shell

| Component | Where | v2 |
|---|---|---|
| Sidebar, `.nav-item` | `layout.html`, 1024 px and up | tokens |
| Tab bar, `.tabbar`, `.tab` | `layout.html`, under 1024 px | tokens |
| More sheet | `layout.html` | tokens |
| Run pill, `.run-pill` | `layout.html` | tokens |
| Page title and lead, `.page-title`, `.page-lead` | Every page | done |

## Controls

| Component | Where | v2 |
|---|---|---|
| Buttons, `.btn`, `.btn-primary`, `.btn-quiet`, `.icon-btn` | Everywhere | tokens |
| Chips, `.chip`, `.lab-chip` | Docks, filters | tokens |
| Inputs, `.input` | Forms | tokens |
| Segmented, `.segmented`, `.decide-types` | Models, Admin, Decisions | tokens |
| Sheets, `.sheet`, `sheet-head`, `confirm` | Everywhere | tokens |
| Composer, `.composer-box`, `.lab-dock`, `lab-field` | Chat, Studio tools | tokens |

## Lists and content

| Component | Where | v2 |
|---|---|---|
| Grouped lists, `.list-group`, `.list-row`, `.list-label` | Settings, More, Models | tokens |
| Model rows, `model-row`, `.model-avatar` | Models, Running | tokens |
| Cards, `.card` | Details, Running | tokens |
| Pills and badges, `.pill`, `.badge` | Models, Running | tokens |
| Notices, `.notice` | Everywhere | tokens |
| Empty states, `empty-state`, `.lab-empty` | Voices, Admin, Running, Studio tools | tokens |
| Skeleton, `.skeleton` | Trays | tokens |

## Studio

| Component | Where | v2 |
|---|---|---|
| Landing, `.studio-card`, `.studio-tile`, `tool-icon` | Studio | done |
| Picture gallery and viewer, `.image-*`, `.viewer` | Image | done |
| Video item | Video | tokens |
| Speech item, waveform, `.speech-*` | Speech | tokens |
| Recording panel, transcript, `.transcribe-*`, `.transcript-*` | Transcribe | tokens |
| Decision result, batch, `.decide-*` | Decisions | done |

## Not built yet

First-run pages, sign-in landing, no-access, expired, 404 and error pages; the model chip and picker in Chat's header; the v2 icon set (`icon-*` in the templates are still v1 drawings).
