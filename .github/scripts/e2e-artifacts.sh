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

version=8.30.1
want=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb
tool=$RUNNER_TEMP/e2e-gitleaks
mkdir -p "$tool"
curl -fsSL --retry 3 -o "$tool/gitleaks.tar.gz" "https://github.com/gitleaks/gitleaks/releases/download/v$version/gitleaks_${version}_linux_x64.tar.gz"
[ "$(sha256sum "$tool/gitleaks.tar.gz" | cut -d' ' -f1)" = "$want" ] || { echo "e2e-artifacts: gitleaks checksum mismatch" >&2; exit 1; }
tar -xzf "$tool/gitleaks.tar.gz" -C "$tool" gitleaks

"$tool/gitleaks" dir "$dir" --no-banner --redact || refuse "possible secret"
if grep -rIhoE '/(home|Users)/[A-Za-z0-9._-]+' "$dir" | sort -u | grep -vE '^/(home|Users)/runner$'; then
  refuse "a path names a user other than the runner"
fi
printf 'e2e-artifacts: %s files, %s KB, clean\n' "$(find "$dir" -type f | wc -l | tr -d ' ')" "$(du -sk "$dir" | cut -f1)"
