# Upgrading and rolling back

## Upgrade

Run the [install script](install.md) again, then apply the new binary:

```sh
sh install.sh
local-base restart
local-base status
local-base smoke      # optional: sends a real request, so it needs a model installed
```

The script replaces the binary by rename and keeps the old one as `~/.local/bin/local-base.previous`. **Always replace the binary by rename, not by copying over it.** macOS caches code signatures per inode, so an in-place copy can make the kernel kill the binary on every run.

`restart` also rewrites the service definition, so prefer it to `launchctl kickstart` or `systemctl --user restart`.

## Roll back

```sh
mv -f ~/.local/bin/local-base.previous ~/.local/bin/local-base
local-base restart
rm -f ~/.local/share/local-base/runs/*.json
```

Use your own data directory in the last line if you set `--root` or `LOCALBASE_ROOT`.

**Why delete `runs/`:** `<root>/runs/` holds checkpoints of detached runs, including model output in plaintext (directory 0700, files 0600). A build with detached runs deletes them on its own, but older binaries ignore that directory, so they would stay on disk until a newer build starts again.

**What a rollback loses:** runs in flight die, and features the older binary lacks stop being available. Pages it doesn't have, such as `/app/studio/*`, return errors, so bookmarks to them break.

## Database compatibility

Within the Go releases so far, rollback is a binary swap:

- The Go schema version (`go_schema`) is 1, and the drizzle migration journal matches 0.1.0's.
- Go-only tables (`imported_models`, `model_backend_selections`, `go_settings`, `model_placements`) stay in place, unused, after a rollback, and are used again when you return.
- Back up the data directory's `local-base.db` before any upgrade you can't afford to redo.

This was drilled on a scratch root with the Go 0.2.0 tree, an earlier Go build and the TypeScript 0.1.0 binary: each served `/health` and `/v1/models` on the same database. That drill requested no inference. A rollback on a copy of a long-lived install from this exact tree hasn't been rehearsed.

### Rolling back to the TypeScript 0.1.0 binary

It starts on the same database only if the active models' files are installed; otherwise it refuses to start. It also lacks everything added in Go, including the fit-aware default model, imports and backend selection, placement, detached runs and the studios. Its data stays in place for your return.
