#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fails when the git index holds a build output, a binary file or a large file
# that is not allowlisted, or when a workflow lacks least-privilege permissions
# or uses an action that is not pinned by commit SHA.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
allowlist=.github/repo-guard-allowlist.txt
max_bytes=$((1024 * 1024))
failed=0

fail() {
  printf 'repo guard: %s\n' "$1" >&2
  failed=1
}

allowed() {
  [ -f "$allowlist" ] && grep -v '^#' "$allowlist" | grep -qxF -- "$1"
}

files() {
  git -c core.quotepath=off ls-files "$@"
}

while IFS= read -r path; do
  case "$path" in
    go/localbase | go/local-base | go/fakebackend | dist/* | .design/*)
      fail "$path must not be committed (build output or local research)"
      ;;
  esac
done < <(files)

while IFS= read -r path; do
  allowed "$path" || fail "$path is binary; add it to $allowlist only with a reason"
done < <(files --eol | awk -F'\t' '$1 ~ /^i\/-text/ { print $2 }')

while IFS=$'\t' read -r size path; do
  if [ "$size" -gt "$max_bytes" ] && ! allowed "$path"; then
    fail "$path is $size bytes (limit $max_bytes); add it to $allowlist only with a reason"
  fi
done < <(
  files -s | awk -F'\t' '{ split($1, meta, " "); print meta[2] "\t" $2 }' |
    while IFS=$'\t' read -r object path; do
      printf '%s\t%s\n' "$(git cat-file -s "$object")" "$path"
    done
)

for workflow in .github/workflows/*.yml; do
  [ -e "$workflow" ] || continue
  grep -qE '^permissions:' "$workflow" || fail "$workflow has no top-level permissions block"
  while IFS= read -r action; do
    case "$action" in
      ./*) ;;
      *@*)
        printf '%s\n' "${action##*@}" | grep -qE '^[0-9a-f]{40}$' ||
          fail "$workflow uses $action, which is not pinned by commit SHA"
        ;;
      *) fail "$workflow uses $action, which is not pinned by commit SHA" ;;
    esac
  done < <(sed -nE 's/^[[:space:]-]*uses:[[:space:]]*["'\'']?([^[:space:]"'\'']+).*/\1/p' "$workflow")
done

if [ "$failed" -ne 0 ]; then
  exit 1
fi
printf 'repo guard: ok\n'
