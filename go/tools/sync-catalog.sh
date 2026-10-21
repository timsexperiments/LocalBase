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
