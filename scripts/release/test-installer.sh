#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Exercises scripts/install.sh and the archive for this machine against a local
# release directory, with HOME and every path inside a throwaway directory.
#
#   scripts/release/test-installer.sh DIST [PORT]
#
# PORT defaults to 26010. Pick any free port if that one is busy.
set -euo pipefail

dist="$(cd "$1" && pwd)"
port=${2:-26010}
repo="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/lb-installer-test.XXXXXX")"
serve_pid=
# The gateway's root is inside $work and it keeps writing while it stops (logs, the database's
# checkpoint), so delete only once it has exited: up to 10 s, then KILL as a last resort.
cleanup() {
  if [ -n "$serve_pid" ]; then
    kill "$serve_pid" 2> /dev/null || true
    for _ in $(seq 1 100); do
      kill -0 "$serve_pid" 2> /dev/null || break
      sleep 0.1
    done
    kill -KILL "$serve_pid" 2> /dev/null || true
    wait "$serve_pid" 2> /dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

export HOME="$work/home"
mkdir -p "$HOME"
unset LOCALBASE_ROOT LOCALBASE_INSTALL_DIR LOCALBASE_LICENSE_DIR LOCALBASE_VERSION
bin="$HOME/.local/bin/local-base"
installer() { LOCALBASE_RELEASE_URL="file://$1" sh "$repo/scripts/install.sh"; }
step() { printf '== %s\n' "$1"; }
fail() { printf 'FAILED: %s\n' "$1" >&2; exit 1; }

step "fresh install verifies the checksum and installs without sudo"
installer "$dist" > "$work/out.log"
[ -x "$bin" ] || fail "no binary at $bin"
[ -f "$HOME/.local/share/doc/local-base/LICENSE" ] || fail "no license"
[ -f "$HOME/.local/share/doc/local-base/THIRD_PARTY_NOTICES.txt" ] || fail "no notices"
[ ! -e "$bin.previous" ] || fail "fresh install left a previous binary"
grep -q 'local-base start' "$work/out.log" || fail "no start hint"
[ ! -e "$HOME/.local/share/local-base" ] || fail "the installer wrote into the default data directory"
version="$("$bin" --version)"
want="${LOCALBASE_EXPECT_VERSION:-$version}"
[ "$version" = "$want" ] || fail "version $version, want $want"

step "the README flow initializes the default data directory after installing"
"$bin" --non-interactive init > /dev/null || fail "init on the default data directory failed after install"
[ -f "$HOME/.local/share/local-base/.localbase-root.json" ] || fail "no root marker in the default data directory"

step "upgrade keeps the previous binary and replaces by rename"
printf 'previous' >> "$bin"
before="$(cksum < "$bin")"
installer "$dist" > "$work/out.log"
[ "$(cksum < "$bin.previous")" = "$before" ] || fail "previous binary not kept"
"$bin" --version > /dev/null || fail "upgraded binary does not run"
[ ! -e "$HOME/.local/bin/.local-base.new" ] || fail "temp file left behind"

step "tampered archive is refused and the installed binary is untouched"
before="$(cksum < "$bin")"
bad="$work/bad"
mkdir "$bad"
cp "$dist"/* "$bad/"
for archive in "$bad"/local-base-*; do printf 'x' >> "$archive"; done
if installer "$bad" > "$work/out.log" 2>&1; then fail "tampered archive was accepted"; fi
grep -q 'checksum mismatch' "$work/out.log" || fail "wrong error: $(cat "$work/out.log")"
[ "$(cksum < "$bin")" = "$before" ] || fail "binary changed"

step "missing checksum entry is refused"
cp "$dist"/local-base-* "$bad/"
printf '0000  other\n' > "$bad/checksums.txt"
if installer "$bad" > "$work/out.log" 2>&1; then fail "missing entry was accepted"; fi
grep -q 'checksum entry missing' "$work/out.log" || fail "wrong error: $(cat "$work/out.log")"

step "the installed binary initializes, reports and serves on a scratch root"
if curl -s -m 1 -o /dev/null "http://127.0.0.1:$port/"; then fail "port $port is already in use; pass a free one"; fi
root="$work/root"
"$bin" --root "$root" --non-interactive init > /dev/null
"$bin" --root "$root" --non-interactive doctor > /dev/null
"$bin" --root "$root" --non-interactive serve --host 127.0.0.1 --port "$port" > "$work/serve.log" 2>&1 &
serve_pid=$!
healthy=
for _ in $(seq 1 40); do
  if health="$(curl -fsS -m 1 "http://127.0.0.1:$port/health" 2> /dev/null)"; then healthy=1; break; fi
  sleep 0.25
done
[ -n "$healthy" ] || { cat "$work/serve.log" >&2; fail "gateway did not answer"; }
kill -0 "$serve_pid" 2> /dev/null || { cat "$work/serve.log" >&2; fail "gateway exited; something else answered on port $port"; }
printf '%s' "$health" | grep -q "\"version\":\"$version\"" || fail "/health version: $health"
code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/v1/models")"
[ "$code" = 401 ] || fail "/v1/models without a key returned $code, want 401"

printf 'installer test passed: %s on %s/%s\n' "$version" "$(uname -s)" "$(uname -m)"
