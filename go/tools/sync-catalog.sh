#!/bin/sh
# Copies the built-in catalog from the TS tree, in src/catalog/models/index.ts
# order (the order breaks recommendation ties). Run via `go generate ./...`.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
src="$here/../src/catalog/models"
dst="$here/internal/catalog/data"
rm -rf "$dst" && mkdir -p "$dst"
grep -oE 'from "\./[a-z]+/[^"]+\.json"' "$src/index.ts" | sed -E 's/from "\.\/(.*)"/\1/' > "$dst/order.txt"
while read -r rel; do
  mkdir -p "$dst/$(dirname "$rel")"
  cp "$src/$rel" "$dst/$rel"
done < "$dst/order.txt"
# Former built-in entries (src/catalog/former, #368), in FORMER_CATALOG_DATA
# order: kept as user entries by installs that still use them.
former="$here/../src/catalog/former"
mkdir -p "$dst/former"
grep -oE 'from "\./[a-z]+/[^"]+\.json"' "$former/index.ts" | sed -E 's/from "\.\/(.*)"/\1/' > "$dst/former/order.txt"
while read -r rel; do
  mkdir -p "$dst/former/$(dirname "$rel")"
  cp "$former/$rel" "$dst/former/$rel"
done < "$dst/former/order.txt"
