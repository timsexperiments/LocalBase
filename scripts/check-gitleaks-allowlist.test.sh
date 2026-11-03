#!/bin/sh
# scripts/check-gitleaks-allowlist.sh accepts a good file and refuses each bad one.
set -u
check=$(dirname "$0")/check-gitleaks-allowlist.sh
status=0
expect() { # name, wanted exit, file content
  printf '%s\n' "$3" | sh "$check" /dev/stdin > /dev/null 2>&1
  got=$?
  if [ "$got" -ne "$2" ]; then
    echo "$1: exit $got, want $2" >&2
    status=1
  fi
}
expect "a reason and one value" 0 "  # A made-up value.
  '''PLANTED_0001''',"
expect "an escaped dot" 0 "  # A metadata key.
  '''some\\.key''',"
expect "no reason" 1 "regexes = [
  '''PLANTED_0001''',"
expect "a second entry with no reason" 1 "  # A made-up value.
  '''PLANTED_0001''',
  '''PLANTED_0002''',"
expect "a pattern" 1 "  # Too wide.
  '''PLANTED_.*''',"
expect "a bare dot" 1 "  # A pattern too.
  '''some.key''',"
expect "no entries" 1 "regexes = ["
exit "$status"
