#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Checks browser-suite output before it's uploaded. The repository is public, so anyone
# can download these artifacts. On a finding it deletes the directory, so nothing uploads.
# It can't read the PNG captures, which show screens over fake backends.
#   .github/scripts/e2e-artifacts.sh DIR
set -euo pipefail

dir=$1
[ -d "$dir" ] || { echo "e2e-artifacts: $dir doesn't exist" >&2; exit 1; }
refuse() { echo "e2e-artifacts: $1, so nothing will be uploaded" >&2; rm -rf "$dir"; exit 1; }

# gitleaks at the version and checksum in scripts/tools.pins.
repo="$(cd "$(dirname "$0")/../.." && pwd)"
tool=$RUNNER_TEMP/e2e-tools
LOCALBASE_TOOLS_DIR=$tool "$repo/scripts/bootstrap.sh" gitleaks > /dev/null || { echo "e2e-artifacts: could not install gitleaks" >&2; exit 1; }

"$tool/bin/gitleaks" dir "$dir" --no-banner --redact || refuse "possible secret"
if grep -rIhoE '/(home|Users)/[A-Za-z0-9._-]+' "$dir" | sort -u | grep -vE '^/(home|Users)/runner$'; then
  refuse "a path names a user other than the runner"
fi
printf 'e2e-artifacts: %s files, %s KB, clean\n' "$(find "$dir" -type f | wc -l | tr -d ' ')" "$(du -sk "$dir" | cut -f1)"
