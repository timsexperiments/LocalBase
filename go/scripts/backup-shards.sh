#!/bin/bash
# Prints the -run pattern for one shard of internal/backup's -race tests, after checking that the
# shards and BACKUP_FILE_TESTS together run every test in the package exactly once. go.yml's
# backup-race jobs run it; it reads no list of its own, so a new test lands in a shard by itself.
#
#   scripts/backup-shards.sh <shard> <count>     shard counts from 0
#
# The tests BACKUP_FILE_TESTS names run without -race elsewhere, and there must be exactly
# BACKUP_FILE_COUNT of them. The rest are sorted by name and dealt round robin. Each shard's
# pattern is then listed back with go test -list, so what is checked is what -run selects: a test
# in no shard or in two, or a package holding fewer tests than BACKUP_TEST_FLOOR, stops every shard.
set -euo pipefail
cd "$(dirname "$0")/.."
shard=${1:?usage: scripts/backup-shards.sh <shard> <count>}
count=${2:?usage: scripts/backup-shards.sh <shard> <count>}
: "${BACKUP_FILE_TESTS:?}" "${BACKUP_FILE_COUNT:?}" "${BACKUP_TEST_FLOOR:?}"
pkg=./internal/backup
fail() { echo "backup-shards: $*" >&2; exit 1; }

# What -run selects for a pattern: tests, fuzz targets and examples, sorted. Benchmarks never run.
selected() {
  local out
  out=$(go test -race -list "$1" "$pkg") || fail "go test -list failed for $1"
  grep -E '^(Test|Fuzz|Example)' <<<"$out" | sort || true
}

all=$(selected '.*')
total=$(grep -c . <<<"$all" || true)
[ "$total" -ge "$BACKUP_TEST_FLOOR" ] || fail "internal/backup lists $total tests, fewer than the floor of $BACKUP_TEST_FLOOR"

files=$(selected "$BACKUP_FILE_TESTS")
nfiles=$(grep -c . <<<"$files" || true)
[ "$nfiles" -eq "$BACKUP_FILE_COUNT" ] || fail "BACKUP_FILE_TESTS matches $nfiles tests, not $BACKUP_FILE_COUNT"

race=$(comm -23 <(echo "$all") <(echo "$files"))
pattern() { echo "^($(awk -v k="$1" -v n="$count" 'NR % n == k' <<<"$race" | paste -sd'|' -))\$"; }

# Every shard's pattern, listed back, plus the files list: each test exactly once, and nothing else.
ran=$files
for ((k = 0; k < count; k++)); do
  got=$(selected "$(pattern "$k")")
  [ -n "$got" ] || fail "shard $k of $count is empty"
  ran=$(printf '%s\n%s\n' "$ran" "$got")
done
ran=$(grep . <<<"$ran" | sort)
twice=$(uniq -d <<<"$ran")
[ -z "$twice" ] || fail "these run more than once: $(paste -sd' ' - <<<"$twice")"
missing=$(comm -23 <(echo "$all") <(uniq <<<"$ran"))
[ -z "$missing" ] || fail "these run in no shard: $(paste -sd' ' - <<<"$missing")"
extra=$(comm -13 <(echo "$all") <(uniq <<<"$ran"))
[ -z "$extra" ] || fail "these are run but not listed: $(paste -sd' ' - <<<"$extra")"

echo "backup-shards: $total tests, $nfiles without -race, the rest in $count shards; shard $shard has $(awk -v k="$shard" -v n="$count" 'NR % n == k' <<<"$race" | grep -c .)" >&2
pattern "$shard"
