#!/bin/sh
# Fuzz the parsers for a bounded time. `go test` already runs each target's seed
# corpus; this script is the on-demand search for new inputs.
#
#   tools/fuzz.sh            # every target for FUZZTIME (default 60s)
#   FUZZTIME=10m tools/fuzz.sh
#
# A failing input is written to the target's testdata/fuzz directory: commit it with the fix.
set -eu
cd "$(dirname "$0")/.."
time=${FUZZTIME:-60s}
for target in \
	./internal/gateway:FuzzStructuredOutput \

	./internal/jsjson:FuzzParse \
	./internal/memory:FuzzGGUFHeader \
	./internal/models:FuzzInspectVariant; do
	pkg=${target%%:*}
	name=${target##*:}
	echo "== $name ($pkg) for $time"
	go test "$pkg" -run '^$' -fuzz "^$name\$" -fuzztime "$time"
done
