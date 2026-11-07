#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Stages what a soak run may publish into $RUNNER_TEMP/soak-upload, and refuses if
# anything in it looks like a secret or names a real user. The repository is public, so
# anyone can download these artifacts.
#   .github/scripts/soak-artifacts.sh RESULT_DIR...
#
# From a failing run's scratch root it takes only an explicit list of logs and run
# files, so whatever the lab adds later stays out. It must never take bin/, which holds
# a lab-tagged build, or models/, which are huge sparse files.
#
# The scan excuses one shape only, in .github/soak-artifacts.gitleaks.toml: a key ID in
# auth_principal_id or api-key:. Known gap, accepted: a secret made only of lowercase hex
# and dashes, 36 characters or fewer, in one of those two fields would pass. Key IDs are
# identifiers, not credentials. Don't widen that rule.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
scratch=$RUNNER_TEMP/soak-scratch
upload=$RUNNER_TEMP/soak-upload
rm -rf "$upload"
mkdir -p "$upload"

for result in "$@"; do
  if [ -d "$result" ]; then cp -R "$result" "$upload/"; fi
done

if [ -d "$scratch" ]; then
  (
    cd "$scratch"
    find . -path './lblab-*' -type f \( \
      -path './lblab-*/*/serve.log' -o \
      -path './lblab-*/*/race.*' -o \
      -path './lblab-*/*/host.json' -o \
      -path './lblab-*/*/root/logs/*' -o \
      -path './lblab-*/*/root/runs/*' \) -print > "$RUNNER_TEMP/soak-upload.list"
    mkdir -p "$upload/scratch"
    tar -cf - -T "$RUNNER_TEMP/soak-upload.list" | tar -xf - -C "$upload/scratch"
  )
fi

# gitleaks at the version and checksum in scripts/tools.pins, for this machine.
tool=$RUNNER_TEMP/soak-tools
LOCALBASE_TOOLS_DIR=$tool "$repo/scripts/bootstrap.sh" gitleaks > /dev/null || { echo "soak-artifacts: could not install gitleaks" >&2; exit 1; }

# Anything found here blocks the upload. Logs show rules and files, never values.
"$tool/bin/gitleaks" dir "$upload" --no-banner --redact --config "$repo/.github/soak-artifacts.gitleaks.toml" ||
  { echo "soak-artifacts: possible secret, so nothing will be uploaded" >&2; rm -rf "$upload"; exit 1; }

if grep -rIlE '/(home|Users)/[A-Za-z0-9._-]+' "$upload" | xargs grep -IhoE '/(home|Users)/[A-Za-z0-9._-]+' 2> /dev/null | sort -u | grep -vE '^/(home|Users)/runner$'; then
  echo "soak-artifacts: a path names a user other than the runner, so nothing will be uploaded" >&2
  rm -rf "$upload"
  exit 1
fi

printf 'soak-artifacts: %s files, %s KB, clean\n' "$(find "$upload" -type f | wc -l | tr -d ' ')" "$(du -sk "$upload" | cut -f1)"
