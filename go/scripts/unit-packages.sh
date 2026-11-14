#!/bin/bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Prints the unit list: every package in the module but the suites that run in jobs of their own
# (internal/backup, test/contract, test/system, test/chaos, test/lab and tools/soak). The filter
# names each suite by its place in the module, not by the module's path, so it holds wherever the
# module sits (go/ today, the top after the move).
#
# It then checks its own answer another way: go list names those suites' import paths from their
# directories, and one still in the list fails the run, as does an empty list. So a filter that
# stops matching (a renamed module, a moved suite) can't let a suite into the unit run unnoticed.
set -euo pipefail
cd "$(dirname "$0")/.."
suites=(./internal/backup ./test/contract ./test/system ./test/chaos ./test/lab ./tools/soak)
list=$(go list ./... | grep -v -E '/(internal/backup|test/(contract|system|chaos|lab)|tools/soak)$')
[ -n "$list" ] || { echo "unit-packages: the unit list is empty" >&2; exit 1; }
for p in $(go list "${suites[@]}"); do
  if printf '%s\n' "$list" | grep -qxF "$p"; then
    echo "unit-packages: $p is in the unit list, but it runs in a job of its own" >&2
    exit 1
  fi
done
printf '%s\n' "$list"
