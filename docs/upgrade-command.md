# The upgrade command

```bash
local-base upgrade --check      # is there a newer version? Changes nothing
local-base upgrade              # install it
local-base upgrade rollback     # go back
```

Nothing installs by itself, ever. **This command is not usable yet:** it needs a release signing key, and until one exists it refuses to install anything and says so. `--check` works today. To update now, re-run the installer.

**Not run on a real installed service yet.** Its stop, swap, restart and rollback have run under systemd in a disposable container, and not under launchd on a Mac or on anyone's real install.

## Checking for a newer version

`local-base upgrade --check` works any time, enables nothing and writes nothing. It makes one request:

```text
GET https://api.github.com/repos/timsexperiments/LocalBase/releases/latest
```

It sends `Accept` and `User-Agent: local-base`, and nothing about your computer: no version, machine id, usage or token. The comparison happens on your computer. GitHub still sees your IP address and the time, as with any request.

**A daily check is available, and off by default:**

```bash
local-base upgrade check-updates on
```

On, the gateway makes that same request at most once in 24 hours, and Settings says when a newer version exists. GitHub limits unauthenticated requests, so a failure stays quiet, blocks nothing, and waits for the next day. It never installs anything.

## What an upgrade does

1. **It checks first, and changes nothing if it refuses:** a development build, a Homebrew install (`brew upgrade local-base`), a directory it can't write, a gateway running in the foreground (stop it first), or a service using a different binary.
2. **If the gateway is busy, it stops and tells you,** with two ways on: `--wait <seconds>` waits for its work to finish, up to 600 s, and `--force` goes ahead and cuts it.
3. **It refuses an older release** unless you pass `--allow-downgrade`.
4. **It verifies what it downloads:** the release's signature over `checksums.txt` first, then the archive against that file, then the binary again immediately before using it. There is no flag to skip this.
5. **It stops the old gateway** and confirms it and its backends are gone. If something survives, it brings the old version back and refuses.
6. **It keeps the old binary** as `local-base.previous`, and replaces the binary with a rename, never writing over it.
7. **It restarts through the service manager, and runs `local-base smoke`.**

If the new version doesn't start, it goes back automatically. If it starts but smoke fails, the new version stays, because smoke can fail for reasons of its own, such as no model installed. It prints what failed and how to go back.

With no gateway on the root, it only replaces the binary, and tells you how to start one.

## Going back

```bash
local-base upgrade rollback
```

The version it replaces becomes `.previous`, so this can be undone the same way. If the new binary won't run at all, this needs nothing from it:

```bash
mv ~/.local/bin/local-base.previous ~/.local/bin/local-base && local-base restart
```

Going back is a binary swap, so it is safe unless a release changed the database in a way an older version can't read. The changelog calls those out. If the older version won't start, rollback brings the newer one back and says so.

## What the signature protects against

It protects against a tampered download, a mirror, or someone altering release files without the key. It can't protect against a stolen signing key before it is revoked, or a compromised release workflow.

If a key is lost or compromised, a new release adds a new key, revoking the old one if needed. If nothing you trust can vouch for the new key, re-run the installer.
