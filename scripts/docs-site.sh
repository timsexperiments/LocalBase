#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds the documentation site with mdBook from the files people already read, so
# nothing is written twice. It needs `make bootstrap`.
#   scripts/docs-site.sh [--strict] [OUTPUT]     default output: .cache/docs-site/book
#
# --strict fails on a page or link that doesn't resolve. Without it, those are warned
# about and pages that haven't landed yet are left out of the navigation.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
export PATH="$repo/.cache/tools/bin:$PATH"
strict=0
if [ "${1:-}" = --strict ]; then
  strict=1
  shift
fi
work=$repo/.cache/docs-site
output=${1:-$work/book}
source=$work/src
blob=https://github.com/timsexperiments/LocalBase/blob/main
command -v mdbook > /dev/null 2>&1 || { echo "docs-site: mdbook is missing; run make bootstrap" >&2; exit 1; }
rm -rf "$source"
mkdir -p "$source"

cp "$repo"/docs/*.md "$source/"
# The site's own pages, unless a real one has landed in docs/.
for page in "$repo"/docs/site/*.md; do
  name="$(basename "$page")"
  [ -e "$source/$name" ] || cp "$page" "$source/$name"
done
cp "$repo/docs/site/book.toml" "$work/book.toml"

# Pages kept at the repository root, whose links are relative to it.
root_page() {
  /usr/bin/env perl -pe '
    s{\]\(docs/}{](}g;
    s{\]\(\.\./}{](}g;
  ' "$repo/$1" > "$source/$2"
}
root_page CONTRIBUTING.md contributing.md
root_page DEVELOPMENT.md development.md
root_page ROADMAP.md roadmap.md
root_page CHANGELOG.md changelog.md
root_page SECURITY.md security-policy.md

# The quickstart is the README's section, so the two can't drift apart.
{
  awk '/^## Quickstart/ { on = 1; print "# Quickstart"; next } on && /^### |^## / { exit } on { print }' "$repo/README.md"
} | /usr/bin/env perl -pe 's{\]\(docs/}{](}g; s{\]\(go/}{]('"$blob"'/go/}g' > "$source/quickstart.md"

# Links that leave the site, and the pages that moved.
for page in "$source"/*.md; do
  BLOB=$blob /usr/bin/env perl -i -pe '
    s{\]\(\.\./README\.md(#[^)]*)?\)}{]($ENV{BLOB}/README.md$1)}g;
    s{\]\(README\.md(#[^)]*)?\)}{]($ENV{BLOB}/README.md$1)}g;
    s{\]\(\.\./THIRD_PARTY_NOTICES\.md\)}{]($ENV{BLOB}/THIRD_PARTY_NOTICES.md)}g;
    s{\]\(\.\./(go|packaging|scripts|src|\.github)/}{]($ENV{BLOB}/$1/}g;
    s{\]\((go|packaging|scripts|src|\.github|LICENSE|REUSE\.toml)(/|\))}{]($ENV{BLOB}/$1$2}g;
    s{\]\(CONTRIBUTING\.md}{](contributing.md}g;
    s{\]\(DEVELOPMENT\.md}{](development.md}g;
    s{\]\(ROADMAP\.md}{](roadmap.md}g;
    s{\]\(CHANGELOG\.md}{](changelog.md}g;
    s{\]\(\.\./CHANGELOG\.md}{](changelog.md}g;
    s{\]\(SECURITY\.md}{](security-policy.md}g;
    s{\]\(SUPPORT\.md\)}{]($ENV{BLOB}/SUPPORT.md)}g;
    s{\]\(CODE_OF_CONDUCT\.md\)}{]($ENV{BLOB}/CODE_OF_CONDUCT.md)}g;
    s{\]\(THIRD_PARTY_NOTICES\.md\)}{]($ENV{BLOB}/THIRD_PARTY_NOTICES.md)}g;
  ' "$page"
done

problems=0
problem() {
  printf 'docs-site: %s\n' "$1" >&2
  problems=$((problems + 1))
}

# Navigation: pages that haven't landed yet can't be built.
while IFS= read -r line; do
  target="$(printf '%s\n' "$line" | sed -nE 's/.*\]\(([^)]+\.md)\).*/\1/p')"
  if [ -n "$target" ] && [ ! -e "$source/$target" ]; then
    problem "navigation names $target, which doesn't exist"
    continue
  fi
  printf '%s\n' "$line"
done < "$repo/docs/site/SUMMARY.md" > "$source/SUMMARY.md"

# Internal links.
for page in "$source"/*.md; do
  while IFS= read -r target; do
    file=${target%%#*}
    [ -n "$file" ] || continue
    [ -e "$source/$file" ] || problem "$(basename "$page") links to $target, which doesn't exist"
  done < <(grep -oE '\]\([^)#: ]+\.md(#[^) ]*)?\)' "$page" | sed -E 's/^\]\(//; s/\)$//' || true)
done

if [ "$strict" = 1 ] && [ "$problems" -gt 0 ]; then
  echo "docs-site: $problems problems" >&2
  exit 1
fi

mdbook build "$work" --dest-dir "$output" 2>&1 | grep -v '^$' || true
[ -f "$output/index.html" ] || { echo "docs-site: mdBook wrote no index.html" >&2; exit 1; }
printf 'Built %s pages into %s, with %s problems.\n' "$(find "$output" -name '*.html' | wc -l | tr -d ' ')" "${output#"$repo"/}" "$problems"
