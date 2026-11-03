# Tools

Most scripts here say what they do at the top. This covers what the reliability and
performance tools share. `docs/reliability.md` and `docs/performance.md` explain the tools
themselves.

| Run from `go/` | What it does |
|---|---|
| `make soak` (`tools/soak.sh`) | A real gateway over fake backends, through memory phases, drain and restart |
| `make chaos` (`tools/chaos`) | What breaks, and what a person sees when it does |
| `make bench` (`tools/bench.sh`) | What the gateway adds per request and per token, against budgets |
| `make e2e` (`tools/e2e.sh`) | Headless Chrome: journeys and a matrix of pages, widths and themes |
| `tools/fetch-ci-tools.sh <dir>` | The lint and secret-scan tools at CI's versions |

Run the secret scan the way CI does, from the repository root. CI runs both of these, and
one passing says nothing about the other:

- `gitleaks dir . --no-banner --redact --config .gitleaks.toml` scans the files as they stand.
- `gitleaks git . --no-banner --redact --config .gitleaks.toml --log-opts=<base>..HEAD` scans
  every commit in the pull request, so changing a value now does not clear an older commit.

A fixture that looks like a secret should not need an allowlist entry: generate it in the
test, or name it `PLANTED_*`. An entry in `.gitleaks.toml` is one exact made-up value with a
one-line reason above it, which `scripts/check-gitleaks-allowlist.sh` enforces. It is for what
is already in a commit and cannot be changed.

`.github/soak-artifacts.gitleaks.toml` is a different file with a different rule: it scans only
generated soak artifacts and uses patterns on purpose, because a key id differs every run. The
gap is written in its header, and the enforcement step reads only `.gitleaks.toml`.

## Disk

Keep 90 GB free on a development machine. A script, test or mutation run that can write large
files must refuse to start under that floor and must cap what it writes, because a timeout
skips a test's cleanup. A mutation that disabled a size limit once left 136 GB behind.

Source `tools/disk-guard.sh` and call `disk_guard` near the top of a bash script. It refuses
under the floor and caps each file the script and its children write (64 GiB by default;
`LOCALBASE_FILE_CAP_GB`). `LOCALBASE_DISK_FLOOR_GB` moves the floor, which is 5 GB in CI. A test
server that sends without end must stop at a bound well above the limit it tests.

## Where things go

- **Built tools:** `~/.cache/localbase/`, or `LOCALBASE_TOOLS_CACHE` if set. They are
  replaced by the next run, so nothing needs cleaning up.
- **The browser suite's Chrome and axe-core:** `~/.cache/localbase-e2e/`, or
  `LOCALBASE_E2E_CACHE` if set.
- **Results:** a new directory under `$TMPDIR`, or `-out <dir>`.
- **Ports:** the lab uses 29000-29499, or `LOCALBASE_LAB_PORTS=<low>-<high>`. Go tests use
  `LOCALBASE_TEST_PORTS=<low>-<high>`, where the tree has `internal/testutil/ports.go`.

Nothing is installed system-wide.

## Pointing a tool at Chrome (`LBGO_CHROME`)

**Start any Chrome for testing with `--use-mock-keychain --password-store=basic`.** A current
Chrome on macOS encrypts cookies with a key from the Keychain. Without those flags it can
stall forever on its first navigation to an `http://` page in the default profile, and it
can put a Keychain prompt on the screen of whoever owns the machine. Measured on Chrome for
Testing 156: `Page.navigate` still had not returned after 40 s without them, and took 2-3 ms
with them. They are also what Puppeteer and Playwright pass.

For any script that starts Chrome:

- Pass both flags, whichever Chrome `LBGO_CHROME` names.
- Give each run a new `--user-data-dir` in scratch, and a debugging port from your own block.
- Start it in its own process group, and stop that group on every way out, including a
  watcher for when the script itself is killed. `tools/e2e/main.go` shows all of this.

A tab in its own `Target.createBrowserContext` happens not to stall, because that context
keeps no cookies on disk. Don't rely on that: it doesn't cover anything else in the browser
that wants the key. Use the flags.

## Anything fetched

**Nothing fetched is unpacked or run until its checksum has matched, and the first mismatch
stops the script.** That applies to scripts here and to any one-off fetch:

- Pin the sha256 in this repository where you can, and compare against it exactly.
- If the checksum comes from an upstream list, match the exact file name. A pattern can also
  match a neighbour, such as the same archive's `.sbom.json`.
- Use `set -e`, or an explicit exit, so that a failed compare stops everything after it.
  Printing a warning and carrying on is how a bad binary gets run.

## Deletes

`go test -count=1 ./tools/rmguard` fails on any script or command file with `rm` on a variable, a
glob, `/dev`, `/`, `~`, or an absolute path outside an explicit scratch prefix. Run it before you
commit. Prefer no delete at all: build to a fixed path that the next run replaces.

These scripts already delete that way. They work, and none of this is urgent, so fix them when you
are next in the file, and take the line out of `knownToDelete` in `tools/rmguard/rmguard_test.go`,
or lower its count, when you do. A file may not gain one, and no other file may join.

| Owner | File: deletes |
|---|---|
| FE thread | `axe-check.sh`: 2, `browser-run.sh`: 3, `ui-shots/run.ts`: 1 |
| Go thread | `contract.sh`: 1, `ios-check.sh`: 2, `otelcol-check.sh`: 1, `placement-rollback-check.sh`: 1, `rehearsal.sh`: 1, `shadow.sh`: 6, `sync-catalog.sh`: 1, `systemd-check.sh`: 1, `tailwind.sh`: 1 |
| Features thread | `upgrade-rehearsal-systemd.sh`: 1 |
