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
| Settings, API keys, users, roles, policies, imported models, backend choices, voice records, usage totals (the database) | Hugging Face token, OTLP headers. **API keys are hashes only**, so they can't be read back. A restored root accepts each key's secret as it was when the backup was made |
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
| What the last check for a newer version found | Not a setting. A file could use it to show a version and link of its own as available. A restored root with the check on asks again |
| Old files from an earlier restore | They hold secrets |
| SQLite's working files (`local-base.db-wal`, `-shm`, `-journal`) | The backup copies through SQLite, so it holds only finished writes |

## Leaving secrets out

```bash
local-base backup --exclude-secrets
```

Secrets are removed from the copy, never from your root. The finished file holds none of them, though a working copy holds the token and OTLP headers for a moment, in the private working directory. Because a blank secret would stop the gateway, a sign-in provider that needs one is left out whole, as are the SMTP settings and download credentials. The file lists each by name, with the command that puts it back, and `restore` prints that list.

**This does not anonymise.** The file still holds users' email addresses, API key hashes, voice recordings and usage totals.

## Restoring

**Only restore a backup you made yourself and have kept private.** A backup isn't signed, and **restoring one decides who can sign in and which programs run as you**. The checks and the report below don't make a file from anyone else safe, and neither does `--dry-run`. See [what this can't protect against](#what-this-cant-protect-against).

Restore checks the whole file before changing anything. It refuses a file you don't own or that others can change, and a database LocalBase couldn't have written: one with a trigger, a view, a table, column or index LocalBase doesn't make, one of its own defined differently, or rows pointing at something that isn't there. It also refuses a file bigger than a root can hold (see [limits](#limits)), and download credentials that fail the checks `auth-profiles add` makes. Before it asks, it shows:

- **Every custom backend's whole command line.** These run as you when their model loads.
- **Who can sign in, by the values that decide it:** each sign-in provider's issuer, what everyone who signs in gets, the address people sign in at, and the mail server that can read sign-in links, each set against what this root has. A provider's label, such as `oidc:google`, is only the file's own name for it.
- **What each role gives, and who has it:** each account, address, domain and invitation, with anyone new to this root first, and anyone who would lose a role. A long list ends with how many more, and `--json` has them all.
- **"A restore signs everyone out. Active API keys keep working, except as said here, and revoked ones stay revoked."** Then which keys would work, which are held back, and which stay as this root has them, as described below.
- **Some of the settings that change who can reach it, or where it reaches:** the gateway's address and who can reach it, the OTLP endpoint, the import settings and connection caps. Backends only ever listen on this computer, whatever their stored host says, because they have no authentication.
- **Which secrets it holds,** found in its files rather than taken from its own list, and how to put back each one it leaves out. If its own list leaves one out, the report says so, since the file may have been changed after it was made.
- **Catalog entries this root will trust as its own,** with where each downloads from. One that changes a built-in entry says which fields, and if it changes where the model downloads from, where it downloaded from before.
- **Download credentials, and where each is sent:** header names and origins, never values.
- **What this root does by itself:** whether it checks GitHub once a day for a newer version, and whether it marks synthesized speech as made by AI, each set against what this root does now. If the file turns marking off, the last lines before the question say so, since telling people the speech was made by AI then becomes your responsibility.
- **How many role bindings and invitations each sign-in provider has,** and how many name no provider. A provider left out with `--exclude-secrets` keeps its bindings, and they do nothing until it is added back.
- **With `--replace`, a user removed or disabled on that root stays that way,** whatever the backup says.

If who can sign in or who has a role differs from this root, the last lines before the question say so, or say that it may when part of this root can't be read.

**The report shows what the file says for what it is.** A control character shows as an escape such as `\x1b`, and so does a letter from outside ASCII, such as `\u0430`, so one address or program can't pass for another. Your key names and the root's path keep their letters. `--json` carries the values as they are.

**This root's keys win.** A restore compares each key's secret, permissions, expiry, name, and whether it's revoked, and keeps this root's version of all of it:

- **A key changed on this root keeps its current secret and permissions,** so a key you rotated because it leaked stays rotated. A key this root doesn't have stays out, whether it was removed here or the file added it. Keys made here since the backup stop working, and the report lists them.
- **On a new root, a root with no keys, or one whose database can't be read,** the backup's ordinary keys come back: the ones that can only make requests, as a new key can. A key that can do more, even one that can only read keys, stays out until you name it, because a fresh root is where a file could bring in an administrator's key with nothing to show it was never there.
- **To take the backup's version of a key, name it:** `--bring-back-key <id>`, once for each key. The report lists each one, and naming a key revoked on this root says that it would work again. Naming a key that would change nothing is refused, since it may be a mistyped id.
- **A key held back can still come back after the restore.** The last lines give the command: the same restore again, with `--bring-back-key`, so it signs everyone out and moves the current files aside again.

**Download credentials are checked the way every download checks them.** The `huggingface` profile, whose origins also get the Hugging Face token, can name only Hugging Face's own hosts. A backup whose `huggingface` profile names others still restores, and those origins are left out wherever it's used, with a warning in the gateway's log.

It refuses a root with a running gateway (stop it first), a root or directory another user owns, a directory that isn't a LocalBase root, a database with a write that never finished, and running as root. A root with data needs `--replace`.

**`--replace` only replaces what a backup holds.** Your models and logs stay. The old files go to `<root>/.before-restore-<time>/`, private to you, and nothing reads them. **They hold secrets too,** so the last line of the output gives the command to delete them. They can include a `local-base.db-journal` that SQLite has finished with, which is safe to delete with the rest.

**If anything fails, everything is put back.** If a restore is interrupted, every command refuses until you run:

```bash
local-base restore --undo
```

Everything comes back byte for byte except the database, whose rows and pages are the same. A restore takes the database out of write-ahead logging to move it, which changes part of its header, and the next command to open it puts that back.

**What you answered is checked again.** After you say yes, the restore holds the root's database, and compares the root's API keys, users, roles and sign-in settings with what the report said. If any of them changed, if the database can't be read, or if something else keeps it open for more than a minute, the restore stops and says so, with nothing changed.

**A crash can leave a write-ahead log with finished writes in it.** Those go into the database before it moves, so the old files hold the same rows, though not the same files. A root that couldn't be read for the report is moved as it is, with whatever is beside it.

**Don't put a database back by hand with a `-wal` beside it.** That includes opening it with `sqlite3`. A `-wal` doesn't say which database it belongs to, so another database's replaces what this one holds, and `PRAGMA integrity_check` still passes. Move `local-base.db-wal`, `-shm` and `-journal` along with the database. `restore` and `restore --undo` do.

**It starts nothing.** The settings carry their ports, so a backup restored beside a running gateway names that gateway's ports. It warns about any that are in use, by connecting to each and closing the connection: change them with `local-base config apply` before starting. `--no-port-probe` skips this, and says which ports it did not check.

Files are created 0600 and directories 0700, owned by you, and the restored root passes the file-permission and database checks a gateway runs at startup.

## Limits

- **A root that doesn't fit isn't backed up.** One file can be at most 8 GiB, and the whole backup 16 GiB and 20,000 files. `backup` says what went over, and keeps nothing.
- **A file with a path over 512 bytes is left out,** and listed with what else was left out.
- **A restore holds each file to what a root can write,** and refuses one over its limit before unpacking anything: the database 8 GiB, a voice 12 MiB, `catalog.d/.former-built-ins` 1 MiB, any other file 8 MiB, and all those other files 64 MiB together. In the database, a value can be at most 1 MiB, and a table at most 100,000 rows and 16 MiB, except `usage_hourly`, where only each value is limited. So a small compressed file can't make a restore, or a dry run, use gigabytes of memory.
- **If a backup of your own root goes over one of these,** `backup` says so and keeps nothing. That root is bigger than LocalBase expects, so please open an issue saying which limit it named.
- **Each backup is whole.** There's no incremental backup, and nothing deletes old backups for you.
- **Models aren't in it,** so a restored root installs them again. Restore lists which.
- **A newer backup can't go into an older LocalBase.** Restore refuses, and asks you to upgrade first.

## What has been tested

Tests cover this on scratch data with a fake backend and tiny models, and one run used real data: **`backup` and `restore` with `--exclude-secrets` on a clone of a real install, at commit `ffc89a72`: a quiet root, with no journal and no writer.** All five commands passed, its schema check gave no false refusal, and it left no temporary files. Handling a busy database and a journal has run only in tests. On 2026-11-03, backup, dry run, restore and `--replace` passed again on a copy of that install at `af6a85b8`, which added the check for rows pointing at nothing, as #603's description records. On 2026-11-05, the later comparison of each table's whole definition was measured against that install's definitions, read from a copy, and they match what it expects, as #650's description records, but no restore has run with it. Later commits have not been through a full run.

**Not tested:**

- A backup with its secrets on real data, including a real `ui-access.json`.
- Custom backends and email delivery on real data. That install has neither, so only scratch data has been through them.
- Restoring on a second computer.
- Signing in with a real provider after a restore.
- Installing a real model after a restore.
- A busy database or a journal on real data. That clone had neither, so these have run only in tests, on real SQLite files, with a held lock standing in for a writer and no process actually killed.

## Troubleshooting

| It says, or you see | What to do |
| --- | --- |
| `A gateway is running on <root>. Stop it first…` | Run the `stop` it names, then restore. `stop` also disables the service, so run the `start` it names afterwards |
| `A gateway started with local-base serve is running on <root>…` | Stop it where it is running, with Ctrl-C or by ending that process, then restore. `local-base stop` only controls the service, and refuses this gateway |
| `This is the default root, the one your installed LocalBase uses.` | Added to either of those. Stopping it stops the LocalBase you normally use, so make sure that's what you mean |
| `<root> already holds a LocalBase root. To replace its settings, run this again with --replace…` | Add `--replace` if you mean it, or choose an empty `--root` |
| `a restore did not finish`, from any command | A restore was interrupted. Run the `restore --undo` it names |
| `A restore is running on '<root>'…`, from any command | Another restore is working on this root. Wait for it to finish, then run your command again |
| `This root changed while this waited…` | Something changed what the report told you, so nothing was changed. Run the restore again to see what it would do now |
| `Something else is using this root's database…` | Nothing was changed. Stop what's using it or wait, then run the restore again |
| `The database on <root> is busy: the gateway or another command is writing to it…` | Try again in a moment, using the command it gives. Nothing was changed. If it keeps happening, stop the gateway, or whatever else is writing |
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
| `The database has rows pointing at something that isn't there…` | Don't restore it unless you know what changed it. If `backup` says this about your own root, that root can't be backed up until those rows are gone, and no command removes them yet, though the gateway keeps working. Either way, report it privately through [GitHub Security Advisories](https://github.com/timsexperiments/LocalBase/security/advisories/new), as the security policy asks, not in a public issue |
| `The database holds more than a root could…`, or `The backup lists "<file>" with a size a backup can't hold.` | Don't restore a file you didn't make that says this. If you think one was made to mislead you, report it privately through [GitHub Security Advisories](https://github.com/timsexperiments/LocalBase/security/advisories/new), as the security policy asks. If `backup` says it about your own root, see [limits](#limits) |
| `The download credentials in this backup are not valid: …` | It names the profile and why, and a root wouldn't use that profile either. If the backup is yours, replace or remove that profile on the root it came from, with `local-base auth-profiles add` or `remove`, then back up again |
| `--bring-back-key <id>: there's nothing to bring back…` | Check the id against the report's lists of keys. Either the backup doesn't hold that key, or this root's version of it is already the same |
| `This root's database can't be read now…` | Nothing was changed. Fix what the error names, then run the restore again |
| `This backup's database can't be used: its journal mode stayed <mode>…` | Nothing was changed. Try again. If it keeps happening, report it privately through [GitHub Security Advisories](https://github.com/timsexperiments/LocalBase/security/advisories/new), as the security policy asks, since the backup's database may have been changed to cause it |
| `A newer LocalBase wrote this backup…` | Upgrade LocalBase, then restore |
| `doctor` names a `.local-base-temporary-…` directory | A backup or restore was killed, and its files may hold secrets. The next backup or restore removes it, or run the `rm` that `doctor` prints |

## What this can't protect against

- **Plaintext secrets at rest:** a backup holds the same secrets a root holds, and anyone who can read it can use them.
- **A parent directory other users can write to** can swap the root, or the backups directory beside it. Owner and mode checks don't protect against that.
- **A backup someone else made or changed.** Nothing signs a backup, and the checksums find damage, not tampering. Restore refuses what LocalBase couldn't have written, and the report shows what decides who can sign in, who has which role, which keys work, what runs as you, where downloads go and what the root does by itself. A file made to mislead can hold all of that in a form LocalBase could have written, so the report is what you decide on, and:
  - **A command to put a secret back is built from what the file says,** checked and quoted, so read where it would send the secret before you run it.
