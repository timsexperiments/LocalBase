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

version=8.30.1
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) asset=linux_x64 want=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb ;;
  Linux-aarch64) asset=linux_arm64 want=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080 ;;
  Darwin-arm64) asset=darwin_arm64 want=b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5 ;;
  Darwin-x86_64) asset=darwin_x64 want=dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709 ;;
  *) echo "soak-artifacts: no gitleaks pin for this machine" >&2; exit 1 ;;
esac
tool=$RUNNER_TEMP/soak-gitleaks
mkdir -p "$tool"
archive=$tool/gitleaks.tar.gz
curl -fsSL --retry 3 -o "$archive" "https://github.com/gitleaks/gitleaks/releases/download/v$version/gitleaks_${version}_$asset.tar.gz"
if command -v sha256sum > /dev/null 2>&1; then got="$(sha256sum "$archive" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$archive" | cut -d' ' -f1)"; fi
[ "$got" = "$want" ] || { echo "soak-artifacts: gitleaks checksum mismatch" >&2; exit 1; }
tar -xzf "$archive" -C "$tool" gitleaks

# Anything found here blocks the upload. Logs show rules and files, never values.
"$tool/gitleaks" dir "$upload" --no-banner --redact --config "$repo/.github/soak-artifacts.gitleaks.toml" ||
  { echo "soak-artifacts: possible secret, so nothing will be uploaded" >&2; rm -rf "$upload"; exit 1; }

if grep -rIlE '/(home|Users)/[A-Za-z0-9._-]+' "$upload" | xargs grep -IhoE '/(home|Users)/[A-Za-z0-9._-]+' 2> /dev/null | sort -u | grep -vE '^/(home|Users)/runner$'; then
  echo "soak-artifacts: a path names a user other than the runner, so nothing will be uploaded" >&2
  rm -rf "$upload"
  exit 1
fi

printf 'soak-artifacts: %s files, %s KB, clean\n' "$(find "$upload" -type f | wc -l | tr -d ' ')" "$(du -sk "$upload" | cut -f1)"
