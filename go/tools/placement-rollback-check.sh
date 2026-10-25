#!/bin/bash
# Rollback drill for the model_placements table (DESIGN §20.8 item 1): the
# previous Go binary and TS 0.1.0 must start and serve against a database
# that has the table, with go_schema still at version 1. A scratch root
# only: copies of the binaries run there, never from ~/.local/bin, on
# loopback scratch ports. No inference is requested; TS loads the active
# model (the cloned 1.5B, about 1 GB) at start.
#
#   LBGO_PREV_GO=<previous Go binary> LBGO_TS=<TS 0.1.0 binary> tools/placement-rollback-check.sh
set -uo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d /private/tmp/lbgo-rbcheck.XXXXXX)
chmod 700 "$work"
root="$work/root"
fails=0
ok() { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
pick() { while :; do p=$((24000 + RANDOM % 6000)); [ $p = 24173 ] && continue; [ $p -ge 25611 ] && [ $p -le 25613 ] && continue
  lsof -nP -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1 || { echo $p; return; }; done; }
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done; sleep 1; rm -rf "$work"; }
trap cleanup EXIT
[ -x "${LBGO_PREV_GO:-}" ] && [ -x "${LBGO_TS:-}" ] || { echo "set LBGO_PREV_GO and LBGO_TS"; exit 2; }
cp "$LBGO_PREV_GO" "$work/prev-go" && cp "$LBGO_TS" "$work/ts" && chmod 755 "$work/prev-go" "$work/ts"
go build -o "$work/new-go" ./cmd/localbase || exit 1
# A real-shaped root: configured, a real (cloned) model file, a stored key, a placement row.
"$work/new-go" --root "$root" configure --defaults --no-create-key >/dev/null || { echo "configure failed"; exit 1; }
model="${LBGO_MODEL_FILE:-$HOME/.local/share/local-base/models/llm/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf}"
[ -f "$model" ] || { echo "no model file at $model (set LBGO_MODEL_FILE)"; exit 2; }
mkdir -p "$root/models/llm" && cp -c "$model" "$root/models/llm/" && ok "cloned $(basename "$model")"
id=$(basename "$model" .gguf)
"$work/new-go" --root "$root" config show > "$work/doc.toml"
# Only the LLM: TS checks every active model's files at boot.
perl -pi -e "s/^selectedLlmModels = .*/selectedLlmModels = [\"$id\"]/; s/^activeLlmModel = .*/activeLlmModel = \"$id\"/;
  s/^(selected(?:Stt|Tts|Image|Video)Models) = .*/\\1 = []/; s/^(active(?:Stt|Tts|Image|Video)Model) = .*/\\1 = \"\"/" "$work/doc.toml"
"$work/new-go" --root "$root" config apply --file "$work/doc.toml" --restart never >/dev/null && ok "active model $id" || bad "config apply"
"$work/new-go" --root "$root" keys create rollback-check --scopes models:read >/dev/null 2>&1 || true
sqlite3 "$root/local-base.db" "INSERT INTO model_placements (model_id, device_id, mode, gpu_layers, cpu_moe_layers, updated_at) VALUES ('qwen3.5-4b-q4_k_m', 'drill-device', 'custom', 10, NULL, '2026-10-25T00:00:00.000Z')" || bad "insert placement row"
state() { sqlite3 "file:$root/local-base.db?mode=ro" "SELECT (SELECT version FROM go_schema WHERE id = 1) || '/' || (SELECT count(*) FROM model_placements)"; }
[ "$(state)" = "1/1" ] && ok "go_schema 1, one placement row" || bad "state $(state)"
serve() { # serve <label> <binary>
  local label=$1 bin=$2 port; port=$(pick)
  "$bin" --root "$root" serve --no-auth --host 127.0.0.1 --port "$port" --llm-port "$(pick)" --stt-port "$(pick)" --tts-port "$(pick)" \
    --image-port "$(pick)" --video-port "$(pick)" > "$work/$label.log" 2>&1 &
  local pid=$!; pids+=("$pid")
  for i in $(seq 1 60); do curl -s -m 2 "http://127.0.0.1:$port/health" >/dev/null && break; sleep 1; done
  if curl -sf -m 5 "http://127.0.0.1:$port/health" >/dev/null && curl -sf -m 5 "http://127.0.0.1:$port/v1/models" >/dev/null; then
    ok "$label serves /health and /v1/models against the database with model_placements"
  else
    bad "$label did not serve:"; tail -5 "$work/$label.log"
  fi
  kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
  [ "$(state)" = "1/1" ] && ok "after $label: go_schema 1, placement row kept" || bad "after $label: $(state)"
}
serve "previous Go ($("$work/prev-go" --version 2>&1 | head -1))" "$work/prev-go"
serve "TS ($("$work/ts" --version 2>&1 | head -1))" "$work/ts"
serve "this Go build" "$work/new-go"
if [ $fails = 0 ]; then echo "rollback check passed"; else echo "rollback check: $fails failure(s)"; fi
exit $((fails > 0))
