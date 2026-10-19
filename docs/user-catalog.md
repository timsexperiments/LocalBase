# User catalog and Hugging Face models

LocalBase ships a built-in catalog of qualified models. You can add your own
models, or adjust built-in ones, without rebuilding LocalBase.

## Where entries live

User entries live in `<root>/catalog.d/`, one JSON file per model. The default
root is `~/.local/share/local-base`, or whatever `--root` or `LOCALBASE_ROOT`
names. Each file uses the same schema as the built-in entries in
`src/catalog/models/`.

Files are read in name order whenever LocalBase starts or a command runs.
Reading the catalog never touches the network; the remote feed below is
downloaded in the background and read from its cache.

## Tiers and precedence

The effective catalog is built from up to three tiers:

| Tier     | Source                                    | Trust when added |
| -------- | ----------------------------------------- | ---------------- |
| user     | `<root>/catalog.d/*.json`                 | `user`           |
| remote   | the community feed, cached under `<root>` | `community`      |
| built-in | shipped with the binary                   | `qualified`      |

When more than one tier names the same `modelId`, user beats remote, and
remote beats built-in.

## Remote community feed

New catalog models, and what LocalBase has learned about Hugging Face files,
can arrive without a LocalBase release. The repository publishes its catalog
as a versioned feed, [`catalog/feed-v2.json`](../catalog/feed-v2.json), and a
running gateway downloads it about once a day: it checks hourly and
downloads only when the cached copy is more than 24 hours old. Binaries
released before schema version 2 keep reading the models-only
[`catalog/feed.json`](../catalog/feed.json).

- **Only new models.** The feed adds model ids this binary does not ship.
  Entries for built-in ids are ignored, so the feed cannot change a
  qualified entry, and your `catalog.d` files always win over it. The feed
  also keeps entries that left the built-in catalog, so older binaries keep
  finding them; this binary ignores those ids too (see below), and their
  annotations carry `verified` instead of `qualified`.
- **Pinned and checked.** Every entry pins a repository revision and artifact
  checksums, like any catalog entry. The feed document carries a
  `schemaVersion` and a sha256 of its contents (its `models` and
  `annotations`); a feed with a mismatched checksum, a schema version this
  binary cannot read or more than 8 MiB is rejected whole. A newer document
  stays readable by older binaries when it sets `minReaderVersion` to a
  version they know; they ignore the fields they do not understand. A feed
  entry or annotation that does not validate is skipped and reported, and
  the rest still load.
- **Offline fallback.** The last good download is kept in
  `<root>/cache/catalog-feed/`. If a refresh fails, LocalBase keeps using
  it; with no download at all, the built-in catalog is used.
- **Community trust.** Feed models show as `community` from the remote tier.
  `local-base models show <id>` prints the feed URL and entry, the model
  details view names the feed, and `local-base diagnostics` reports the feed
  state and any skipped entries.

### Annotations

Schema version 2 adds `annotations`, keyed by Hugging Face repository and
revision (and optionally the file). Each one can publish:

- **Status**: `qualified` (LocalBase ships and supports this exact revision)
  or `verified` (LocalBase checked it and published what it found).
- **Verified-unsupported capabilities** with a reason, such as a chat
  template with no tool-call format.
- **Measured memory peaks** per variant and launch shape, with the hardware
  and date.
- **A recipe** for a multi-file model (image, video, text-to-speech): every
  file pinned by revision, size and sha256.

Search results (`POST /_localbase/model-discovery` and **Find more models**)
show the annotation beside each file and repository, with an **Install**
action for a recipe. Installed models show it in the model details view and
in `local-base models show <id>`; `local-base models catalog` summarizes it.

Precedence, highest first:

1. **Runtime observation.** A verified tool-call round-trip turns a
   capability back on whatever the feed says.
2. **Your entry.** A mark never removes a capability a `catalog.d` file adds
   or declares; it is shown as "not applied". Adding a file from Hugging Face
   writes the feed's marks into the new entry, where you can edit them.
3. **The feed.** Marks apply only to an exact revision match. An annotation
   for another revision of the same model is shown, never applied.

Not yet done: a recipe installs a catalog entry this binary or the feed
already has; it cannot create a new multi-file entry from an arbitrary
repository (`models add` can, for the image pipelines below).

Published memory peaks also feed the memory fit, as a fallback. A peak is used
only when the annotation matches the pinned revision (and, when the recipe
names one, the primary file's sha256) and was taken at the launch shape
LocalBase would plan. It ranks below this machine's own observed peak and the
binary's checked-in measurements, and above the estimate. The fit and details
show "Measured (catalog feed)" when it applies. Launch admission is unchanged.

Manage the feed with `local-base models feed`:

```sh
local-base models feed status            # URL, last download, schema, counts
local-base models feed refresh           # download now if stale
local-base models feed refresh --force   # download now
local-base models feed use https://example.com/my-feed.json
local-base models feed use off           # built-in and user catalogs only
local-base models feed use default       # back to the published feed
```

Feed URLs must use `https` (plain `http` is allowed only for `localhost`).
`LOCALBASE_CATALOG_FEED_URL` overrides the saved choice; set it to `off` to
turn the feed off. The CLI reads the cached feed but never downloads it
except through `models feed refresh`.

To publish your own feed, use the same document shape. Contributors
regenerate both repository feeds, with annotations built from the catalog
and the checked-in memory measurements, after editing `src/catalog/models/`
with `bun run catalog:feed`; a test fails while either is out of date.

## Same-id semantics: add or patch

- **A new `modelId`** must be a complete, valid model spec. It is added with
  trust `user`.
- **An existing `modelId`** is a patch. Each top-level field in the file
  replaces that field in the lower tier's entry. Nested objects and arrays are
  replaced whole, not merged. The patched entry must still be a valid spec.
- Unknown fields are rejected, so a typo cannot silently do nothing.
- If two files in `catalog.d` name the same `modelId`, the first file in name
  order wins and the other one is reported.

### Trust after a patch

Cosmetic patches, such as `displayName`, `notes` or `catch`, keep the entry's
trust. A patch that changes how the model launches makes the entry `modified`
and drops its reference memory and speed measurements, because those no
longer describe what runs. Patching any of these fields counts as a launch
change:

`kind`, `source`, `repositoryRevision`, `artifacts`, `llmRuntime`,
`ttsRuntime`, `imageRuntime`, `imageLaunchOptions`, `videoRuntime`,
`contextWindowTokens`.

A patch that repeats the existing value changes nothing.

Trust and source are shown by `local-base models list`, `local-base models
show <id>`, the model details view and `local-base diagnostics`.

### Invalid files

An invalid file is skipped with a diagnostic, and the rest of the catalog
still loads. Invalid files include bad JSON, files that are not JSON objects,
invalid specs, unknown fields and files larger than 1 MiB. `local-base
diagnostics` lists every skipped file and the reason.

If a selected model came from a skipped file, the configuration error names
that file so you can fix or restore it.

## Models that left the built-in catalog

The built-in catalog is a small, tested set, and a release can drop an
entry. A dropped entry is never offered for browsing or new installs, but an
install that uses it keeps working: when LocalBase loads the catalog and
finds the entry's primary file under `<root>/models/`, or reads settings
that select it, it writes the spec the earlier release shipped (same id,
pinned revision, files and checksums) to `catalog.d/<id>.json`. From then on
it is a user entry like one added from Hugging Face:

- API clients, defaults and selections keep using the same id.
- Checked-in memory measurements still apply while the entry's files and
  launch shape are unchanged; observed peaks on this machine keep applying
  as before.
- It shows as user trust instead of qualified, and its notes start with
  "No longer in the LocalBase catalog".
- Disable, uninstall or remove it like any user entry. LocalBase records the
  ids it wrote in `catalog.d/.former-built-ins` and never writes one again,
  so `local-base models remove-entry <id>` sticks.

The specs ship in the binary under `src/catalog/former/`.

## Adding a model from Hugging Face

```sh
local-base models search qwen 0.5b
```

```
MODEL                            DOWNLOADS  LICENSE     PIPELINE         GATED  LIKELY KIND
Qwen/Qwen2.5-0.5B-Instruct-GGUF  275.2k     apache-2.0  text-generation  -      llm

Add one with: local-base models add hf:<org>/<repo>[:<file-or-quant>]
```

The search lists repositories LocalBase can run, sorted by downloads. By
default it lists GGUF language models. `--kind stt` lists speech recognition
repositories with whisper.cpp `ggml-*.bin` files, and `--kind image` lists
text-to-image repositories, plus image-to-image and GGUF repositories of a
supported pipeline. `LIKELY KIND` says what LocalBase would run each
result as, or why it cannot. `GATED` marks repositories whose terms you must
accept on Hugging Face before downloading.

```sh
local-base models add hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF          # picks Q4_K_M
local-base models add hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF:Q8_0     # a quant
local-base models add hf:Org/Repo:path/to/file.gguf               # an exact file
local-base models add hf:Org/Repo --install                       # add, then download
local-base models add hf:Org/Repo --id my-model                   # choose the id
local-base models add hf:ggerganov/whisper.cpp:ggml-small.bin     # a whisper model
local-base models add hf:Org/sd-checkpoint:model.safetensors      # an image checkpoint
```

`models add` reads the repository's metadata from the Hugging Face API, then:

- pins the current revision sha;
- records each file's LFS sha256 and size, so the install is checksum-verified;
- records the license and its commercial status, and the pipeline tag;
- includes every part of a split (`-00001-of-0000N.gguf`) model;
- pairs an `mmproj` projector for image-input models;
- writes `<root>/catalog.d/<id>.json`.

`models add` infers the kind from the pipeline tag and the files:

- **Chat (llm):** a `text-generation` or image-text-to-text repository with
  GGUF files. With no selector it uses the only GGUF file, or else the first
  available quant from Q4_K_M, Q4_K_S, Q5_K_M and Q8_0.
- **Transcription (stt):** an `automatic-speech-recognition` repository with
  whisper.cpp `ggml-*.bin` files (Core ML encoders and VAD models are
  skipped). With no selector it prefers a base Q8_0 file, then the smallest
  Q8_0, then the smallest file.
- **Image:** a `text-to-image` repository with a single-file Stable Diffusion
  checkpoint (`.safetensors`, `.ckpt` or `.gguf`) at the repository root,
  which sd-server loads with `-m`. A checkpoint is launched with
  `--prediction v` (`imageLaunchOptions.prediction: "v"`) when its name or
  tags say v-prediction (`v-pred`, `768-v`, SD 2.x 768) or its
  `scheduler/scheduler_config.json` declares `v_prediction`; the entry's notes
  say why.
- **Image pipelines:** repositories of FLUX.1 (dev and schnell, and their
  fine-tunes), FLUX.2 klein 4B and 9B, and Z-Image-Turbo, found by repository
  name, `base_model` tags or `model_index.json`. LocalBase pins the diffusion
  model file from the repository (with no selector, the first of Q4_0, Q4_K,
  Q4_K_S, Q5_0 and Q8_0, else the smallest), and the text encoders and VAE
  from the measured built-in entry for that pipeline, each with its own
  revision and sha256. Files with the same bytes as an installed model are
  shared, not downloaded again. FLUX.1 dev-style models get 20 steps.
- **Image LoRAs:** a `peft` repository, or one tagged `lora` or
  `base_model:adapter:...`, whose `base_model` tags (or, without any, its name)
  point at FLUX.1 schnell, FLUX.2 klein 4B or 9B, Z-Image-Turbo, SDXL 1.0
  base or SD 1.5. The entry pins the LoRA `.safetensors` file from the
  repository root (with no selector, the only file, or the only one without
  `comfy` in its name) plus every file of the matching built-in base entry,
  which is shared rather than downloaded again.
  It records `imageLaunchOptions.lora` with a default strength of 1 and the
  card's `instance_prompt` as the trigger words. Requests to that entry apply
  the LoRA through sd-server; `lora_strength` (0 to 2, 0 turns it off) on
  `/v1/images/generations` and `/v1/images/edits` overrides the strength, and
  the generation settings show a **LoRA strength** field only for entries that
  have one. Other models reject `lora_strength`. LoRAs for FLUX.1 dev, the
  undistilled klein or Z-Image bases, FLUX.2 dev, SDXL turbo or refiner, and
  bases with no LocalBase profile are refused with the reason.
- **Refused, with a reason:** diffusers folder layouts with no single-file
  release, LoRAs for unsupported bases, and pipelines sd-server cannot map from
  one repository (Qwen-Image, SD3, FLUX.1 Kontext, FLUX.2 dev, Wan and other
  video models). Search results show the reason instead of an **Add** button.

If the choice is ambiguous it lists the files and asks you to pick one. Ids
that a built-in or remote entry already uses are refused; pick another with
`--id`.

Capabilities found in the chat template (for example tool calling) are shown
as `detected`. They are confirmed from the installed files and at runtime, not
written into the entry.

Gated or private repositories need a token: `local-base configure
--hf-token`. Hugging Face responses are cached under `<root>/cache/hf/` for
ten minutes. If Hugging Face cannot be reached, the cached answer is used.
With no cache, the command fails and changes nothing. Serving requests never
contacts Hugging Face.

### Finding models in the catalog UI

The catalog page has a **Find more models** view. Search by name and kind
(Chat, Transcription or Image). Each result shows its downloads, license,
pipeline tag and likely kind, and for the most downloaded results, the file
`models add` would pin with its download size and a memory-fit estimate for
this machine. Gated results get a badge and a link to accept the terms on
Hugging Face. **Add** writes the same pinned entry as `models add`; **Add and
install** also starts the download. Adding needs the `models:manage`
permission.

The view calls `POST /_localbase/model-discovery` on the gateway with one of:

```text
{ "action": "search", "query": "qwen 0.5b", "kind": "llm" }
{ "action": "preview", "repo": "Org/Repo" }
{ "action": "add", "repo": "Org/Repo", "selector": "file.gguf", "install": false }
```

The gateway uses the saved Hugging Face token, or else `HF_TOKEN`, and sends
it only to huggingface.co. It is never returned or logged.

Results whose catalog-feed notes describe a revision other than the Hub's
current one say "(other revision)" on the feed badge.

### Hugging Face token

The **Hugging Face token** panel at the top of the view saves, replaces,
checks and clears the token. It shows only whether a token is set and where it
comes from (**Saved** or **From HF_TOKEN**); the value is never shown again.
Saving checks the token with Hugging Face's whoami first and refuses one the
Hub rejects; it still saves when the Hub cannot be reached. Clearing the saved
token falls back to `HF_TOKEN` when it is set. A gated repository, a refused
preview or a Hub rate limit links straight to the panel. The panel needs the
`models:manage` permission and calls `/_localbase/hf-token`:

```text
GET                                       -> { "configured": true, "source": "saved", "envAvailable": false }
POST { "action": "save", "token": "hf_…" } -> status plus { "check": { "state": "valid", ... } }
POST { "action": "validate" }              -> checks the token in use
POST { "action": "clear" }                 -> removes the saved token
```

A token saved here lasts until it is cleared or replaced. `local-base configure`
changes it only when asked:

- `--hf-token <token>` always replaces the saved token, and `--hf-token ""`
  clears it.
- An `hfToken` in a TOML file passed with `--config` is saved only when no
  token is saved yet, or when it equals the saved one. It never overwrites a
  different saved token, such as one saved in this panel, so re-running
  configure with the same file is safe. Configure then keeps the saved token
  and prints a warning, without either value. To switch to the TOML token,
  replace it here or pass `--hf-token`.
- Interactive configure prompts for the token, with the saved one (or
  `HF_TOKEN`) as the default, unless a flag or TOML file sets it.

### Removing an entry

```sh
local-base models remove-entry my-model
```

This deletes the user file. If the id was a patch, the remote or built-in
entry is used again. Downloaded files are left in place. A selected model's
only entry cannot be removed; deselect it with `local-base configure` first.

## Declared unsupported capabilities

An entry can state that a capability is known not to work:

```json
"unsupportedCapabilities": [
  {
    "id": "tool-calling",
    "supported": false,
    "source": "declared",
    "verified": true,
    "reason": "It answers tool requests with plain text instead of tool calls."
  }
]
```

This blocks the capability even when the chat template or the engine's
`/props` say it is supported. Only a successful runtime round-trip overrides
it. Diagnostics and the model details view show the reason.
