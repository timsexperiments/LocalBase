#!/bin/sh
# Fuzz the parsers for a bounded time. `go test` already runs each target's seed
# corpus; this script is the on-demand search for new inputs.
#
#   scripts/fuzz.sh            # every target for FUZZTIME (default 60s)
#   FUZZTIME=10m scripts/fuzz.sh
#
# A failing input is written to the target's testdata/fuzz directory: commit it with the fix.
set -eu
cd "$(dirname "$0")/.."
time=${FUZZTIME:-60s}
for target in \
	./internal/auth:FuzzJWKSVerify \
	./internal/conf:FuzzParseDocument \
	./internal/gateway:FuzzStructuredOutput \
	./internal/gateway/httpx:FuzzMultipart \
	./internal/gateway/infer:FuzzImageChecks \
	./internal/gateway/infer:FuzzRelayStream \
	./internal/gateway/infer:FuzzValidateSpeech \
	./internal/gateway/infer:FuzzValidators \
	./internal/gateway/runs:FuzzRequestModel \
	./internal/gguf:FuzzGGUFHeader \
	./internal/jsjson:FuzzParse \
	./internal/models:FuzzInspectVariant \
	./internal/otlp:FuzzExtract; do
	pkg=${target%%:*}
	name=${target##*:}
	# go test -fuzz passes on a package without the target, so a moved target would fuzz nothing.
	go test -list "^$name\$" "$pkg" | grep -qx "$name" || { echo "fuzz.sh: $pkg has no $name" >&2; exit 1; }
	echo "== $name ($pkg) for $time"
	go test "$pkg" -run '^$' -fuzz "^$name\$" -fuzztime "$time"
done
