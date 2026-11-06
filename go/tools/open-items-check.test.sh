#!/bin/sh
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# tools/open-items-check.sh passes only with the exact heading and no bullets under it, and the
# release workflows and the checklist call it. Runs on every pull request.
set -u
here=$(dirname "$0")
check=$here/open-items-check.sh
repo=$here/../..
status=0
h="### 34.8 Open: a file from someone else isn't safe yet (Security's review, 2026-11-03)"

fail() {
  echo "$1" >&2
  status=1
}

expect() { # name, wanted exit, file content
  printf '%s\n' "$3" | sh "$check" /dev/stdin > /dev/null 2>&1
  got=$?
  [ "$got" -eq "$2" ] || fail "$1: exit $got, want $2"
}

# What is still open fails it.
expect "an open item" 1 "$h

- one"
expect "a star" 1 "$h

* one"
expect "a plus" 1 "$h

+ one"
expect "a number" 1 "$h

1. one"
expect "a number and a bracket" 1 "$h

1) one"
expect "an indented item" 1 "$h

Some words.

  - one"
expect "under a smaller heading" 1 "$h

#### Part of it

- one"
expect "after a code comment, which isn't a heading" 1 "$h

\`\`\`sh
# a comment
\`\`\`

- one"
expect "in a code block, to be safe" 1 "$h

\`\`\`text
- one
\`\`\`"

# Nothing open passes, whatever the rest of the file lists.
expect "the heading and no items" 0 "$h"
expect "words and no items" 0 "$h

Nothing is open."
expect "items before it" 0 "### 34.7 Residual risks

- accepted

$h"
expect "items in the next section" 0 "$h

### 34.9 Something else

- not open"
expect "items in the next chapter" 0 "$h

## 29. Who owns a backend

- not open"
expect "items in the next part" 0 "$h

# Part two

- not open"

# A heading it can't find fails it, so a rename can't turn the check off.
expect "no heading" 2 "## 34. Backup and restore

- one"
expect "an empty file" 2 ""
expect "another title" 2 "### 34.8 Closed: nothing to see

- one"
expect "another title and no items" 2 "### 34.8 Closed: nothing to see"
expect "without the word Open" 2 "### 34.8 A file from someone else isn't safe yet (Security's review, 2026-11-03)"
expect "another number" 2 "### 34.9 Open: a file from someone else isn't safe yet (Security's review, 2026-11-03)"
expect "another level" 2 "## 34.8 Open: a file from someone else isn't safe yet (Security's review, 2026-11-03)"
expect "words after it" 2 "$h and more"
expect "only in a code block" 2 "\`\`\`text
$h
\`\`\`"
expect "twice" 2 "$h

$h"

sh "$check" /nonexistent/DESIGN.md > /dev/null 2>&1
got=$?
[ "$got" -eq 2 ] || fail "a missing file: exit $got, want 2"
sh "$check" "$here" > /dev/null 2>&1
got=$?
[ "$got" -ne 0 ] || fail "a directory: exit 0"

# The real file has the heading. Whether anything is open depends on the day, but it must be checkable.
sh "$check" > /dev/null 2>&1
got=$?
[ "$got" -eq 0 ] || [ "$got" -eq 1 ] || fail "go/DESIGN.md can't be checked: exit $got"

# It is called where a release starts. Each of these fails if the call is removed or moved away.
awk '/if \[ "\$REF_TYPE" = tag \]; then/ { inside = 1 } inside && /^ *fi$/ { inside = 0 }
  inside && /go\/tools\/open-items-check\.sh/ { found = 1 } END { exit !found }' \
  "$repo/.github/workflows/release-go.yml" || fail "release-go.yml doesn't call it for a tag"
awk '/^  preflight:$/ { inside = 1 } /^  qualify:$/ { inside = 0 }
  inside && /go\/tools\/open-items-check\.sh/ { found = 1 } END { exit !found }' \
  "$repo/.github/workflows/release.yml" || fail "release.yml's preflight doesn't call it"
grep -q 'go/tools/open-items-check\.sh' "$repo/docs/release-checklist.md" || fail "the release checklist doesn't name it"

exit "$status"
