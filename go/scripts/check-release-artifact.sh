#!/bin/bash
# Checks built release binaries for the upgrade test key (DESIGN §33.5). release-go.yml runs it on
# every binary it builds, after the build and before anything is signed or packaged. The recipe
# test (cmd's TestReleaseBuildSetsNoTestKey) reads the build scripts; this reads what they made, so
# it also sees a tag or -X that reached the build some other way, such as the CI environment.
#
#   scripts/check-release-artifact.sh <binary>...
#
# A binary fails if:
#   - go version -m names the localbase_testkey tag, or testKey in its recorded flags;
#   - it holds the name upgrade.testKey, which a build that isn't stripped keeps;
#   - it holds a key-shaped string, "<id>:<44 base64 characters>", the form the test key's -X
#     takes. go.sum's h1: hashes have that shape too, so they're left out.
# test/system's TestTheReleaseShapeCarriesNoTestKey runs it on release-shaped builds with and
# without the tag.
set -euo pipefail
[ $# -ge 1 ] || { echo "usage: scripts/check-release-artifact.sh <binary>..." >&2; exit 2; }
command -v go >/dev/null || { echo "check-release-artifact: needs go, for go version -m" >&2; exit 2; }

failed=0
for bin in "$@"; do
  bad=()
  if [ ! -f "$bin" ]; then
    echo "check-release-artifact: $bin: no such file" >&2
    failed=1
    continue
  fi
  if ! info=$(go version -m "$bin" 2>&1); then
    echo "check-release-artifact: $bin: go version -m can't read it: $info" >&2
    failed=1
    continue
  fi
  if grep -q 'localbase_testkey' <<<"$info"; then
    bad+=("go version -m names the localbase_testkey tag: $(grep -m1 'localbase_testkey' <<<"$info" | tr -s '[:space:]' ' ')")
  fi
  if grep -q 'testKey' <<<"$info"; then
    bad+=("go version -m names testKey in its build flags")
  fi
  if LC_ALL=C grep -a -q 'upgrade\.testKey' "$bin"; then
    bad+=("it holds the name upgrade.testKey")
  fi
  keys=$(LC_ALL=C grep -a -o -E '[A-Za-z0-9._-]{1,64}:[A-Za-z0-9+/]{43}=' "$bin" | grep -v '^h1:' | sort -u || true)
  if [ -n "$keys" ]; then
    bad+=("it holds $(grep -c . <<<"$keys") key-shaped string(s), with id(s): $(cut -d: -f1 <<<"$keys" | paste -sd' ' -)")
  fi
  if [ ${#bad[@]} -eq 0 ]; then
    echo "check-release-artifact: $bin: no test key"
  else
    failed=1
    for why in "${bad[@]}"; do echo "check-release-artifact: $bin: $why" >&2; done
  fi
done
exit "$failed"
