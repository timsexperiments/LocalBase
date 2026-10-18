# Three directions

These proposals share the same catalog, trust rules and resource model so the comparison tests navigation and interaction. They are alternatives, not three phases of one implementation. All data about current workloads is illustrative. Refer to BRIEF.md for observed evidence and source limits.

## A. Shelf

Core idea: start with what you own. A stable Library / Get models / Running now structure separates stored assets, acquisition and current memory. The daily picker is a short recent-model list inside chat. One search spans sources; qualification belongs to the selected artifact.

```text
Chat → quick picker → installed, enabled chat models → conversation
                 ↘ get another model
Models
├── Library → model details → enable / defaults / remove
├── Get models → one search → model family → file choice → download → enable
└── Running now → active request or idle → unload idle model
```

Reading order: title and current job, primary action, compact disk/memory summary, inventory. Desktop uses a persistent rail and aligned rows. At 390 px the same destinations sit above the page; rows stack their metadata. The picker is centered on desktop and a native modal bottom sheet on phone. Existing green action and neutral tokens carry the hierarchy.

| Job | Steps |
| --- | --- |
| Daily choose | Open chat's model control → tap a recent installed chat model → return to the preserved draft. No default changes |
| Weekly inspect | Library → scan disk and installed rows → Running now → inspect busy/idle and memory → unload an idle model |
| Occasionally add | Get models → search Qwen3.8 → choose its family → review qualified Q4_K_M or expand ten common files → review bytes → download on Mac → verify → enable explicitly |
| Rare defaults | Library → API defaults → choose capability → manage an enabled model → set default. Chat, decisions and embeddings remain distinct |
| Rare disable/remove | Library → Details → disable or remove. A current default needs replacement. Remove asks for confirmation; unload preserves disk files |
| Recovery | Download → error → retry with same choice. Access expiry → sign in simulation → reconnect to same operation |

Trade-offs: familiar ownership structure, quick daily switching, and enough room for weekly maintenance. Adding a model requires moving away from chat, so preserve the draft and return destination. Library and Running now partially repeat model names, but answer different questions. Many installed models will eventually need richer search and disk sorting.

Risks: "Library" can still be mistaken for a catalog unless every row clearly says Installed. A permanently visible Get models destination may encourage unnecessary browsing. A generic memory summary must never imply that downloading consumes the displayed peak.

Try: concepts/shelf/index.html. Start on Library. Use Explore prototype states for direct entry to a picker, warning or empty library.

## B. Compass

Core idea: begin with intent. "What do you want to do?" offers Chat, Decisions, Transcribe, Speak, Images, Embeddings and Video. A short priority choice leads to a recommendation with a reason. My models and Mac status are secondary destinations. Recent chat models bypass the guide.

```text
Task launchpad
├── Continue recent chat → conversation → inline model chooser
├── Task → priority → recommendation + installed alternatives
│                       → file choice → review → download → enable
├── Search by name → direct results → file choice
├── My models → details / defaults / cleanup
└── Mac status → running / memory / unload
```

Reading order: immediate continuation, task, preference, recommendation, technical detail. Large text and task buttons make the entrance understandable without model names. Desktop guide steps occupy a left column; phone shows compact step labels above the recommendation. The picker is a full page of current and other choices, not an overlay. The direct search escape matters for this technical owner.

| Job | Steps |
| --- | --- |
| Daily choose | Continue the last conversation → model control → inline current/other installed choices → use a model → draft remains |
| Weekly inspect | My models → installed collection and disk → Mac status → busy/idle memory → unload |
| Occasionally add | Pick task → choose Balanced, Small footprint or More fidelity → see a qualified recommendation → review actual file(s) → download → verify → enable. Search by name bypasses the guide |
| Rare defaults | My models → API defaults → capability → model details → set default |
| Rare disable/remove | My models → details → disable/remove, with default guard and explicit removal review |
| Recovery | Error or Access expiry → recover → resume the same recommendation and selected artifact |

Trade-offs: best at explaining how a new capability enters the system. More navigation for an owner who already knows a model ID. Task names help separate Clef and embeddings from chat, but seven choices stretch the phone page. A guide makes an occasional job prominent while weekly management becomes secondary.

Risks: recommendations can look more authoritative than their evidence. "More fidelity" is an intent, not a measured ranking. The prototype deliberately describes the uncertainty and still prefers qualified artifacts. A real recommender needs rules for capability, runtime, host fit and the user's existing collection. Speech tasks need qualified multi-file recipes rather than a generic quant chooser.

Try: concepts/compass/index.html. The task and priority steps work, including direct search and installed alternatives. Compare its inline picker with Shelf's sheet.

## C. Control room

Core idea: model selection is allocating a shared machine. Start with the running workspace, then inspect stored files or provision an asset. A persistent memory strip gives context. A load plan shows current work and proposed changes before any large model runs.

```text
Mac workspace + memory budget
├── Running lanes → request owner / busy / idle → unload idle
├── Choose for chat → inspector picker → reuse loaded or load on demand
├── Disk inventory, largest first → manage / defaults / remove
├── Provision → search → artifact choice → resource review → download only
└── Transfers → progress / verify / enable
Load plan → current request + proposed model + reserve → wait or change choice
```

Reading order: available shared memory, current work, proposed action, disk inventory when needed. Desktop uses running lanes plus a plan inspector. Phone stacks those sections. Its picker is a right-side inspector on desktop, a bottom sheet on phone. Provisioning and transfer management are first-class destinations; source provenance remains within artifact detail.

| Job | Steps |
| --- | --- |
| Daily choose | Chat control or workspace Choose for chat → inspect loaded candidates → select → continue conversation. Per-request scope is explicit |
| Weekly inspect | Workspace → read current owners and busy/idle → unload idle model → Disk inventory → scan largest files first |
| Occasionally add | Provision model → unified search → file choice → review transfer → download only → verify → enable → review fresh memory before loading |
| Rare defaults | Disk inventory → API defaults → capability → model details → set default |
| Rare disable/remove | Disk inventory → Manage → disable/remove. Busy Clef cannot be unloaded in the prototype; removing a default is guarded |
| Recovery | Transfers remains independent of the load plan → retry/reconnect → inspect authoritative operation state. No automatic eviction |

Trade-offs: strongest account of the shared 64 GB Mac and API clients. It makes weekly operational questions unusually clear. The persistent memory header consumes precious phone space and can make a simple chat choice feel like server administration. Provision, inventory and workspace vocabulary needs testing with the owner.

Risks: current free memory is volatile. A plan is not a reservation, and the server must recheck on load. Summing observed peaks can double-count shared/unified allocations. Busy ownership, cancellation, pending work and safe unloading need server support. Do not implement automatic eviction or interrupt another program to make a green bar fit.

Try: concepts/control/index.html. Compare Workspace, Disk inventory, the inspector picker, and Review a load plan.

## Recommendation

Choose Shelf as the base IA. The owner's daily job is choosing for a conversation; the weekly job is understanding the Mac. Shelf gives both a direct place without making occasional acquisition the homepage or requiring resource planning before every chat. Its three model destinations answer plain questions: what do I have, what can I add, what is running?

Bring Compass's task guidance into Shelf's Get models empty/search landing. Bring Control room's explicit current-work explanation into Shelf's load warning. Keep those as focused flows. Do not combine all three navigation systems.

This is an expert recommendation from jobs and desk research. Before implementation, have the owner compare the same four tasks in each prototype. If runtime contention is actually daily, Control room becomes a stronger base. If setup and discovery dominate, Compass deserves another round.
