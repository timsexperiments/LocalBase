#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# A job that can read the Apple signing key or the notarizing password runs nothing from the
# repository or a build, since whatever runs in that job can reach them. It only uses Apple's tools
# on what a build left. Fails naming the job and the line that breaks this.
set -euo pipefail

status=0
for workflow in .github/workflows/*.yml .github/workflows/*.yaml; do
  [ -e "$workflow" ] || continue
  report="$(awk '
    function flush() { if (job != "" && holds && found != "") print job ":" found; job = ""; holds = 0; found = "" }
    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    !in_jobs { next }
    /^[^[:space:]#]/ { flush(); in_jobs = 0; next }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { flush(); job = $1; next }
    /^[[:space:]]*#/ { next }
    /secrets\.(APPLE_CERTIFICATE|APPLE_CERTIFICATE_PASS|APPLE_ID_PASSWORD)([^A-Za-z0-9_]|$)/ { holds = 1 }
    /uses:[[:space:]]*(actions\/checkout|[A-Za-z0-9_.-]+\/setup-[A-Za-z0-9_-]+|actions\/setup-[A-Za-z0-9_-]+)/ { found = found "\n  " $0; next }
    /(^|[^A-Za-z0-9_.\/-])(bun|bunx|npm|npx|node|cmake|make|go|cargo|python|python3|pip|pip3)[[:space:]]/ { found = found "\n  " $0 }
    END { flush() }
  ' "$workflow")"
  if [ -n "$report" ]; then
    printf '%s: a job holding the signing key or notarizing password runs something else:\n%s\n' "$workflow" "$report" >&2
    status=1
  fi
done
exit "$status"
