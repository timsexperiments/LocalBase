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

| Tier     | Source                                     | Trust when added |
| -------- | ------------------------------------------ | ---------------- |
| user     | `<root>/catalog.d/*.json`                  | `user`           |
| remote   | the community feed, cached under `<root>` | `community`      |
| built-in | shipped with the binary                    | `qualified`      |

When more than one tier names the same `modelId`, user beats remote, and
remote beats built-in.

## Remote community feed

New catalog models can arrive without a LocalBase release. The repository
publishes its catalog as a versioned feed,
[`catalog/feed.json`](../catalog/feed.json), and a running gateway downloads
it about once a day: it checks hourly and downloads only when the cached
copy is more than 24 hours old.

- **Only new models.** The feed adds model ids this binary does not ship.
  Entries for built-in ids are ignored, so the feed cannot change a
  qualified entry, and your `catalog.d` files always win over it.
- **Pinned and checked.** Every entry pins a repository revision and artifact
  checksums, like any catalog entry. The feed document carries a
  `schemaVersion` and the sha256 of its `models` array; a feed with a
  mismatched checksum, an unknown schema version or more than 8 MiB is
  rejected whole. A feed entry that is not a valid spec is skipped and
  reported, and the rest still load.
- **Offline fallback.** The last good download is kept in
  `<root>/cache/catalog-feed/`. If a refresh fails, LocalBase keeps using
  it; with no download at all, the built-in catalog is used.
- **Community trust.** Feed models show as `community` from the remote tier.
  `local-base models show <id>` prints the feed URL and entry, the model
  details view names the feed, and `local-base diagnostics` reports the feed
  state and any skipped entries.

Manage the feed with `local-base models feed`:

```sh
local-base models feed status            # URL, last download, revision, counts
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
regenerate the repository feed after editing `src/catalog/models/` with
`bun run catalog:feed`; a test fails while it is out of date.

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

## Adding a model from Hugging Face

```sh
local-base models search qwen 0.5b
```

```
MODEL                           DOWNLOADS  LICENSE     PIPELINE         LIKELY KIND
Qwen/Qwen2.5-0.5B-Instruct-GGUF 275.2k     apache-2.0  text-generation  llm

Add one with: local-base models add hf:<org>/<repo>[:<file-or-quant>]
```

The search lists only GGUF repositories, which are the format LocalBase's LLM
engine runs, sorted by downloads.

```sh
local-base models add hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF          # picks Q4_K_M
local-base models add hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF:Q8_0     # a quant
local-base models add hf:Org/Repo:path/to/file.gguf               # an exact file
local-base models add hf:Org/Repo --install                       # add, then download
local-base models add hf:Org/Repo --id my-model                   # choose the id
```

`models add` reads the repository's metadata from the Hugging Face API, then:

- pins the current revision sha;
- records each file's LFS sha256 and size, so the install is checksum-verified;
- records the license and its commercial status, and the pipeline tag;
- includes every part of a split (`-00001-of-0000N.gguf`) model;
- pairs an `mmproj` projector for image-input models;
- writes `<root>/catalog.d/<id>.json`.

When there is no selector, `models add` uses the only GGUF file if there is
one, or else the first available quant from Q4_K_M, Q4_K_S, Q5_K_M and Q8_0.
Otherwise it lists the files and asks you to pick one.

Capabilities found in the chat template (for example tool calling) are shown
as `detected`. They are confirmed from the installed files and at runtime, not
written into the entry. Only GGUF language models can be added for now.

Gated or private repositories need a token: `local-base configure
--hf-token`. Hugging Face responses are cached under `<root>/cache/hf/` for
ten minutes. If Hugging Face cannot be reached, the cached answer is used.
With no cache, the command fails and changes nothing. Serving requests never
contacts Hugging Face.

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
