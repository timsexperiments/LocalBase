#!/bin/sh
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Nothing is released or tagged while go/DESIGN.md section 34.8 lists anything. Run it before
# tagging: the release workflows run it too, but only once a tag has been pushed, so they stop what
# the tag starts and not the tag.
#
# It fails closed. A heading it can't find exactly, or a file it can't read, is a failure, so renaming
# the section can't turn the check off. To retire it, remove the heading, this script, its test and its
# calls in one pull request.
#
# Exit status: 0 nothing is open, 1 something is open, 2 it could not check.
# Usage: open-items-check.sh [file], by default go/DESIGN.md.
set -u
heading="### 34.8 Open: a file from someone else isn't safe yet (Security's review, 2026-11-03)"
file=${1:-$(dirname "$0")/../DESIGN.md}

# Fences are followed so that a "# comment" in a code block can't end the section early, which would
# hide what comes after it. A bullet-shaped line in a code block still counts, to be safe.
open=$(HEADING=$heading awk '
  /^[ \t]*(```|~~~)/ { fenced = !fenced; next }
  !fenced && $0 == ENVIRON["HEADING"] { seen++; inside = 1; next }
  !fenced && /^##?#? / { inside = 0; next }
  inside && /^[ \t]*([-*+]|[0-9]+[.)])[ \t]/ { print; found = 1 }
  END {
    if (seen != 1) exit 2
    if (found) exit 1
    exit 0
  }
' "$file" 2> /dev/null)
status=$?

case "$status" in
  0)
    echo "Nothing is open in section 34.8 of $file."
    exit 0
    ;;
  1)
    {
      echo "Don't release or tag: section 34.8 of $file still lists:"
      printf '%s\n' "$open"
      echo "Each goes when its fix is in the tree."
    } >&2
    exit 1
    ;;
  *)
    {
      echo "Can't check, so don't release or tag: $file has to hold exactly one line that reads:"
      echo "  $heading"
      echo "If it was renamed on purpose, change this check in the same pull request."
    } >&2
    exit 2
    ;;
esac
