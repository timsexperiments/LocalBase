#!/bin/sh
# Opt-in check against a real OpenTelemetry Collector (not run in CI).
#
#   go/tools/otelcol-check.sh            # from the repository root
#
# Downloads the pinned core otelcol release into a scratch directory,
# verifies its sha256, and runs TestRealCollector: both gateways export to
# the collector on a test port (never 127.0.0.1:4318), its file exporter
# writes what it parsed, and the test compares the two. It needs bun for
# the TypeScript gateway, and the contract suite's model environment:
# LOCALBASE_CONTRACT_MODELS (a directory holding
# qwen2.5-coder-1.5b-instruct-q4_k_m.gguf; the gateways link it read-only)
# and LOCALBASE_CONTRACT_RUNTIMES (a copy of bin/runtimes), which also adds
# a real chat request.
set -eu
if [ -z "${LOCALBASE_CONTRACT_MODELS:-}" ] || [ -z "${LOCALBASE_CONTRACT_RUNTIMES:-}" ]; then
  echo "otelcol-check: set LOCALBASE_CONTRACT_MODELS and LOCALBASE_CONTRACT_RUNTIMES (see the header)" >&2
  exit 2
fi
version=0.163.1
case "$(uname -s)/$(uname -m)" in
Darwin/arm64) asset=otelcol_${version}_darwin_arm64.tar.gz
  sha=664047e6ffd98ffaaf90d282bb481ffad46b1e8456f0b2f65e42d627d665d8d3 ;;
*) echo "otelcol-check: pinned for darwin/arm64 only; add this platform's release and sha256" >&2; exit 2 ;;
esac
dir=${TMPDIR:-/tmp}/lbgo-otelcol-$version
mkdir -p "$dir"
if [ ! -x "$dir/otelcol" ]; then
  dl=$(mktemp -d)
  curl -sSfL -o "$dl/$asset" "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v$version/$asset"
  echo "$sha  $dl/$asset" | shasum -a 256 -c - >/dev/null
  tar -xzf "$dl/$asset" -C "$dir" otelcol
  rm -rf "$dl"
fi
cd "$(dirname "$0")/.."
LOCALBASE_CONTRACT_OTELCOL="$dir/otelcol" exec go test ./contract -count=1 -run 'TestRealCollector' -v -args -impl=both
