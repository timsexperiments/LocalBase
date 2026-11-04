# Backup and restore

```bash
local-base backup                                 # write this root's settings to a private file
local-base backup --dry-run                       # say what it would hold, write nothing
local-base --root ~/restored restore --dry-run <file>   # check a backup, change nothing
local-base --root ~/restored restore <file>       # restore it
```

## Privacy: a backup is a secret

By default it holds your Hugging Face token, OTLP headers, sign-in client secrets, SMTP password and download credentials, **in plain text, and it is not encrypted**. Anyone who can read the file can use them. The command says so every time.

- **It is a private file:** 0600, in a 0700 directory beside the root (`~/.local/share/local-base-backups/`). `--output` chooses another place, but not inside the root or in a directory other users can change, and it never overwrites a file.
- **LocalBase never uploads it.** There is no route, no page and no sync. Nothing in this feature uses the network.
- **To move one anywhere,** encrypt it first with `age` or `gpg`, or keep it on an encrypted disk.
- `uninstall` leaves your backups where they are, and says so.

## What is in it, and what isn't

Nothing is left out silently: `backup` lists everything it leaves out, with its size and why, and that list is in the file too.

**A backup is checked before it is kept.** `backup` reads the file back with the same checks `restore` makes, and if restore would refuse it, it keeps nothing and says why. `--dry-run` does the same work and keeps nothing.

While it works, it uses a private directory named `.local-base-temporary-…`, beside the root or the backup, which holds what the backup holds. It is removed however the command ends, unless the process is killed outright. Then `local-base doctor` names it, and the next backup or restore removes it.

| Included | Secrets in it |
|---|---|
| Settings, API keys, users, roles, policies, imported models, backend choices, voice records, usage totals (the database) | Hugging Face token, OTLP headers. **API keys are hashes only**, so they can't be read back, and a restored root accepts the same keys |
| Sign-in providers (`ui-access.json`) | OIDC and GitHub client secrets |
| Email delivery (`email-delivery.json`) | SMTP password |
| Download credentials (`credentials/auth-profiles.json`) | Header values |
| Catalog entries and custom backends (`catalog.d/`, `backends.d/`) | None that LocalBase knows of. They are copied as they are, so don't put one in them |
| Custom voices (`voices/`) | Not secrets, but recordings of someone's voice |

| Not included | Why |
|---|---|
| `models/` | Large, and downloaded again. Restore lists which to install |
| `runs/` | Replies in progress. No prompt or reply ever goes in a backup |
| `logs/`, `bin/`, `cache/`, `runtime/`, `service/`, `catalog-feed.json` | Not settings, tied to this machine, or fetched again |
| Sessions and sign-in links | A restore signs everyone out |
| Measured memory | Measured on one machine, and measured again on the next load |
| Old files from an earlier restore | They hold secrets |
| SQLite's working files (`local-base.db-wal`, `-shm`, `-journal`) | The backup copies through SQLite, so it holds only finished writes |

## Leaving secrets out

```bash
local-base backup --exclude-secrets
```

Secrets are removed from the copy, never from your root. The finished file holds none of them, though a working copy holds the token and OTLP headers for a moment, in the private working directory. Because a blank secret would stop the gateway, a sign-in provider that needs one is left out whole, as are the SMTP settings and download credentials. The file lists each by name, with the command that puts it back, and `restore` prints that list.

**This does not anonymise.** The file still holds users' email addresses, API key hashes, voice recordings and usage totals.

## Restoring

Restore checks the whole file before changing anything, treating it as input from someone else: a backup isn't signed, and **restoring one decides who can sign in and which programs run as you**. So it refuses a file you don't own or that others can change, and a database holding anything LocalBase doesn't make. Before it asks, it shows:

- **Every custom backend's whole command line.** These run as you when their model loads.
- **"A restore signs everyone out. Active API keys keep working, and revoked ones stay revoked."** Keys are counted by state.
- **Settings that change who can reach it, or where it reaches:** the gateway's address and who can reach it, the OTLP endpoint, the import settings and connection caps. Backends only ever listen on this computer, whatever their stored host says, because they have no authentication.
- **Who gets a role, by sign-in provider,** and what names no provider. A provider left out with `--exclude-secrets` keeps its bindings, and they do nothing until it is added back.
- **With `--replace`, what comes back:** keys, users and role bindings the root no longer has. **A key revoked on that root stays revoked, and a user removed or disabled there stays that way,** whatever the backup says, so an older backup can't bring back a leaked key. A new root has nothing to compare with, and neither does a database that can't be read, which the report says. In either case, check `local-base keys list` afterwards.

It refuses a root with a running gateway (stop it first), a root or directory another user owns, a directory that isn't a LocalBase root, a database with a write that never finished, and running as root. A root with data needs `--replace`.

**`--replace` only replaces what a backup holds.** Your models and logs stay. The old files go to `<root>/.before-restore-<time>/`, private to you, and nothing reads them. **They hold secrets too,** so the last line of the output gives the command to delete them.

**If anything fails, everything is put back.** If a restore is interrupted, every command refuses until you run:

```bash
local-base restore --undo
```

Everything comes back byte for byte except the database, whose rows and pages are the same. A restore takes the database out of write-ahead logging to move it, which changes part of its header, and the next command to open it puts that back.

**What you answered is checked again.** After you say yes, the restore holds the root's database and compares it with what the report said. If it changed, or something else keeps the database open for more than a minute, the restore stops and says so, with nothing changed.

**A crash can leave a write-ahead log with finished writes in it.** Those go into the database before it moves, so the old files hold the same rows, though not the same files. A root that couldn't be read for the report is moved as it is, with whatever is beside it.

**Don't put a database back by hand with a `-wal` beside it.** That includes opening it with `sqlite3`. A `-wal` doesn't say which database it belongs to, so another database's replaces what this one holds, and `PRAGMA integrity_check` still passes. Move `local-base.db-wal`, `-shm` and `-journal` along with the database. `restore` and `restore --undo` do.

**It starts nothing.** The settings carry their ports, so a backup restored beside a running gateway names that gateway's ports. It warns about any that are in use, by connecting to each and closing the connection: change them with `local-base config apply` before starting. `--no-port-probe` skips this, and says which ports it did not check.

Files are created 0600 and directories 0700, owned by you, and the restored root passes the same checks a gateway runs at startup.

## Limits

- **A root that doesn't fit isn't backed up.** One file can be at most 8 GiB, and the whole backup 16 GiB and 20,000 files. `backup` says what went over, and keeps nothing.
- **A file with a path over 512 bytes is left out,** and listed with what else was left out.
- **Each backup is whole.** There's no incremental backup, and nothing deletes old backups for you.
- **Models aren't in it,** so a restored root installs them again. Restore lists which.
- **A newer backup can't go into an older LocalBase.** Restore refuses, and asks you to upgrade first.

## What has been tested

Tests cover this on scratch data with a fake backend and tiny models, and one run used real data: **`backup` and `restore` with `--exclude-secrets` on a clone of a real install, at commit `d4dcfa34`.** All five commands passed, its schema check gave no false refusal, and it left no temporary files. Later commits have not been through that run.

**Not tested:**

- A backup with its secrets on real data, including a real `ui-access.json`.
- Custom backends and email delivery on real data. That install has neither, so only scratch data has been through them.
- Restoring on a second computer.
- Signing in with a real provider after a restore.
- Installing a real model after a restore.
- A database journal on real data. That clone had none, so handling a live or crash-left journal has run only in tests, on real SQLite files with no process actually killed.

## Troubleshooting

| It says, or you see | What to do |
| --- | --- |
| `A gateway is running on <root>. Stop it first…` | Run the `stop` it names, then restore |
| `<root> already holds a LocalBase root. To replace its settings, run this again with --replace…` | Add `--replace` if you mean it, or choose an empty `--root` |
| `a restore did not finish`, from any command | A restore was interrupted. Run the `restore --undo` it names |
| `A restore is running on '<root>'…`, from any command | Another restore is working on this root. Wait for it to finish, then run your command again |
| `This root changed while this waited…` | Something changed what the report told you, so nothing was changed. Run the restore again to see what it would do now |
| `Something else is using this root's database…` | Nothing was changed. Stop what's using it or wait, then run the restore again |
| `The database on <root> is busy: the gateway or another command is writing to it…` | Try again in a moment, using the command it gives. Nothing was changed. If it keeps happening, stop the gateway first |
| `The database on <root> has a write that never finished…` | A process stopped mid-write. Run the `status` it names, which undoes that write, then try again. `backup` and `restore` say this and change nothing |
| `the restore failed, and everything was put back: …` | Nothing changed. Fix what it names, then try again |
| `the restore failed (…), and so did putting things back (…).` | Fix what it names, such as a full or failing disk, then run the `restore --undo` it names. Until then, every command refuses |
| `Refusing to use <file>: other users can change it…` | If you trust the file, run the `chmod go-w` it names |
| `Refusing to use <file>: another user owns it`, `it is a symbolic link` or `it has another hard link` | Restore reads only a plain file with one name that you own, because a backup decides who can sign in. If you trust the file, copy it to a new one |
| `Refusing to use <root>: another user owns it.` | If the root is yours, run the `chown` it names. If you did not put it there, leave it |
| `Refusing to restore as root…` | Run it as the user who will own the root |
| `other users can reach <directory>, and a backup holds secrets.` | Run the `chmod 700` it names, or choose a place with `--output` |
| `<file> already exists, and a backup never overwrites a file` | Choose another name with `--output` |
| `This needs <n> bytes of disk, and <n> are free.` | Free up that much space |
| `This backup would not restore, so it was not kept: …` | Fix what it names, which comes from the same checks `restore` makes |
| `The backup is damaged`, or `… does not match its checksum.` | The file changed after it was written, and can't be repaired. Use another backup |
| `The database holds what LocalBase does not make, so it was changed by something else: …` | Don't restore it unless you know what changed it, because restoring decides who can sign in and what runs as you |
| `A newer LocalBase wrote this backup…` | Upgrade LocalBase, then restore |
| `doctor` names a `.local-base-temporary-…` directory | A backup or restore was killed, and its files may hold secrets. The next backup or restore removes it, or run the `rm` that `doctor` prints |

## What this can't protect against

- **Plaintext secrets at rest:** a backup holds the same secrets a root holds, and anyone who can read it can use them.
- **A parent directory other users can write to** can swap the root, or the backups directory beside it. Owner and mode checks don't protect against that.
