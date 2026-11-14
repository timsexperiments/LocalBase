#!/bin/bash
# What DESIGN §35.6 could not answer without a real embedding model: how much one input can hold,
# how long batches take, whether vectors arrive normalized, whether a query prefix helps, and memory
# before and after. One bounded pass, in a window the operator has granted:
#   LOCALBASE_REAL_WINDOW=go tools/real-embedding-check.sh <scratch dir>
# Rehearsal, with the fake backend and a stub for live /health, touching nothing live:
#   tools/real-embedding-check.sh --dry-run <scratch dir>
#
# DOCUMENTED EXCEPTION TO "NEVER THE LIVE PORT". Inside a granted window only, the watcher may GET
# http://127.0.0.1:2273/health every 3 s, and nothing else on the live port: no other path, no POST,
# no live llama-server port. It stops everything if the live modalities state changes or free memory
# falls under the floor. Before the window, use vm_stat or memory_pressure only, and the operator
# does any live check.
#
# THE ONLY OTHER TOUCH OF THE LIVE ROOT is a read-only `cp -c` of two things, once, inside the window:
#   ~/.local/share/local-base/models/llm/Qwen3-Embedding-0.6B-Q8_0.gguf
#   ~/.local/share/local-base/bin/runtimes/llama-server/<the directory this build pins>
# Nothing runs from the live root. The clone of the runtime is what runs.
#
# Limits the operator set: qwen3-embedding-0.6b-q8_0, default reserve, ports in 28000-28499. The gateway
# planner's exact plan is printed BEFORE any real load, from a separate scratch root with a fake
# backend (so no peak recorded there can skew the real root), and the pass aborts if it is over 3.5 GiB.
# At least 46% free to start, 40% floor throughout (free + speculative + inactive pages).
#
# THIS RUNS 2 SLOTS AT CONTEXT 16384, not the planner's default of 4 slots at context 32768, whose plan
# on the real file was not measured. Each slot has the same 8192 tokens as the default, so what one
# input can hold should not change, but memory and batch timing are not the default's. On the real
# file, 2 slots at 16384 plan 3261 MiB (derived).
#
# Adapted from Reliability's tools/real-memory-check.sh.
# This script deletes nothing. It prints the paths to delete afterwards.
set -u
case "${1:-}" in
  --dry-run) DRY=1; shift ;;
  *) DRY=0 ;;
esac
if [ $# -ne 1 ]; then
  echo "usage: LOCALBASE_REAL_WINDOW=go tools/real-embedding-check.sh <scratch dir>, or --dry-run <scratch dir>" >&2
  exit 2
fi
if [ "$DRY" = 0 ] && [ "${LOCALBASE_REAL_WINDOW:-}" != go ]; then
  echo "real-embedding-check: only in a window the operator has granted: set LOCALBASE_REAL_WINDOW=go" >&2
  exit 2
fi
cd "$(dirname "$0")/.." || exit 2
mkdir -p "$1" || exit 2
W=$(cd "$1" && pwd)/real-embedding
if [ -e "$W" ]; then
  echo "real-embedding-check: $W already exists; delete it first" >&2
  exit 2
fi
mkdir -p "$W" || exit 2
EV=$W/evidence.txt
CSV=$W/memory.csv
RESULTS=$W/results.json
: > "$EV"

MODEL_ID=qwen3-embedding-0.6b-q8_0
GW=28410 LLM=28411
CTX=16384 SLOTS=2
PLAN_LIMIT_MIB=3584 # 3.5 GiB
START_PCT=46
FLOOR_PCT=40
HARD_SECONDS=900
LIVE=http://127.0.0.1:2273/health
if [ "$DRY" = 1 ]; then
  # A rehearsal's own stub takes the place of live /health. These can only change it.
  LIVE=http://127.0.0.1:28412/health
  START_PCT=${DRY_START_PCT:-0}
  FLOOR_PCT=${DRY_FLOOR_PCT:-0}
  PLAN_LIMIT_MIB=${DRY_PLAN_LIMIT_MIB:-$PLAN_LIMIT_MIB}
  HARD_SECONDS=${DRY_HARD_SECONDS:-$HARD_SECONDS}
fi
case " $GW $LLM " in *" 2273 "* | *" 18000 "* | *" 18080 "*) echo "a live port" >&2; exit 2 ;; esac

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$EV"; }
pages_mib() {
  vm_stat | awk '/page size of/ { page = $8 } /Pages (free|speculative|inactive)/ { gsub(/\./, "", $NF); n += $NF } END { printf "%d\n", n * page / 1048576 }'
}
free_pct() { echo $(( $(pages_mib) * 1048576 * 100 / $(sysctl -n hw.memsize) )); }
live_state() { curl -s -m 3 "$LIVE" | jq -c '.modalities // "unreachable"' 2>/dev/null || echo unreachable; }
SCRATCH_ENV=(env -i PATH=/usr/bin:/bin:/usr/sbin "HOME=$W/home" LOCALBASE_CATALOG_FEED_URL=off
  HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 NO_PROXY=127.0.0.1,localhost)
# A rehearsal of a slow model load: the fake waits this long before it listens.
if [ "$DRY" = 1 ] && [ -n "${DRY_FAKE_STARTUP_MS:-}" ]; then SCRATCH_ENV+=("FAKE_STARTUP_MS=$DRY_FAKE_STARTUP_MS"); fi

GWPID="" STUB="" MON="" DOG="" WORK="" SAMPLER=""
sweep() {
  ps -A -o pid=,command= | awk -v p="$W/" 'index($2, p) == 1 { print $1 }' | while read -r pid; do
    kill -KILL "$pid" 2>/dev/null
  done
}
stop_gateway() {
  [ -n "$GWPID" ] || return 0
  kill -TERM "$GWPID" 2>/dev/null
  for _ in $(seq 1 120); do kill -0 "$GWPID" 2>/dev/null || break; sleep 0.25; done
  kill -KILL "$GWPID" 2>/dev/null
  wait "$GWPID" 2>/dev/null
  GWPID=""
}
finish() {
  local code=$1
  # Ignored, not reset: the watcher's signal can arrive during this, and with the default action it
  # would kill the script here and leave the gateway running. A rehearsal found exactly that.
  trap '' INT TERM HUP
  trap - EXIT
  [ -n "$MON" ] && kill "$MON" 2>/dev/null
  [ -n "$DOG" ] && kill "$DOG" 2>/dev/null
  [ -n "$WORK" ] && kill "$WORK" 2>/dev/null
  [ -n "$SAMPLER" ] && kill "$SAMPLER" 2>/dev/null
  # On any stop that isn't a clean finish, the backend goes first and at once: that is what holds the
  # memory. A rehearsal showed a gateway asked to stop during a load can take over 30 s to go.
  if [ "$code" != 0 ]; then
    ps -A -o pid=,command= | awk -v p="$W/" -v g="$W/local-base" 'index($2, p) == 1 && $2 != g { print $1 }' | while read -r pid; do
      kill -KILL "$pid" 2>/dev/null
    done
  fi
  stop_gateway
  sweep
  [ -n "$STUB" ] && kill "$STUB" 2>/dev/null
  log "END: exit $code; running from the scratch path: $(ps -A -o pid=,command= | awk -v p="$W/" 'index($2, p) == 1' | wc -l | tr -d ' ')"
  log "END: free memory $(free_pct)%"
  log "END: nothing was deleted. When done, delete by exact path: $W"
  exit "$code"
}
trap 'log "stopped by a signal"; finish 3' INT TERM HUP
trap 'finish $?' EXIT

# Every port must be free first. A stale listener here could answer in place of this run's gateway
# or backend, or in a rehearsal, in place of its stub.
for port in "$GW" "$LLM" 28412; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN > /dev/null 2>&1; then
    log "REFUSED: port $port is already in use"
    exit 4
  fi
done

log "plan: $MODEL_ID, context $CTX, $SLOTS slots; limit $PLAN_LIMIT_MIB MiB; start at $START_PCT% free, floor $FLOOR_PCT%; hard stop $HARD_SECONDS s; dry run $DRY"

CATALOG=internal/catalog/data/llm/$MODEL_ID.json
FILE=$(jq -r '.artifacts[] | select(.role == "primary") | .filename' "$CATALOG")
SIZE=$(jq -r '.artifacts[] | select(.role == "primary") | .expectedSizeBytes' "$CATALOG")
[ -n "$FILE" ] && [ -n "$SIZE" ] || { log "no primary file for $MODEL_ID in the catalog"; exit 5; }

if [ "$DRY" = 1 ]; then
  mkdir -p "$W/stub"
  echo '{"modalities":{"llm":{"configured":true,"state":"idle"}}}' > "$W/stub/health"
  (cd "$W/stub" && exec /usr/bin/python3 -I -m http.server 28412 --bind 127.0.0.1 > /dev/null 2>&1) &
  STUB=$!
  disown "$STUB"
  for _ in $(seq 1 80); do curl -s -m 1 -o /dev/null "$LIVE" && break; sleep 0.1; done
fi

# The window's first look at live, and at the floor, before anything starts.
BASE=$(live_state)
PCT=$(free_pct)
log "PREFLIGHT: live modalities $BASE; free memory $PCT%"
case "$BASE" in
  *'"llm":{"configured":true,"state":"running"}'* | *'"llm":{"configured":true,"state":"idle"}'*) ;;
  *) log "PREFLIGHT FAILED: live llm is not idle or running"; exit 4 ;;
esac
if [ "$PCT" -lt "$START_PCT" ]; then
  log "PREFLIGHT FAILED: free memory $PCT% is under the $START_PCT% this pass needs to start"
  exit 4
fi

(
  trap - EXIT INT TERM HUP
  while sleep 3; do
    now=$(live_state)
    if [ "$now" != "$BASE" ]; then
      echo "[$(date +%H:%M:%S)] LIVE CHANGED: $BASE -> $now" >> "$EV"
      kill -TERM $$
      exit 0
    fi
    pct=$(free_pct)
    if [ "$pct" -lt "$FLOOR_PCT" ]; then
      echo "[$(date +%H:%M:%S)] FREE MEMORY UNDER THE FLOOR: $pct% < $FLOOR_PCT%" >> "$EV"
      kill -TERM $$
      exit 0
    fi
  done
) &
MON=$!
disown "$MON"
( trap - EXIT INT TERM HUP; sleep "$HARD_SECONDS"; echo "[$(date +%H:%M:%S)] WATCHDOG after $HARD_SECONDS s" >> "$EV"; kill -TERM $$ ) &
DOG=$!
disown "$DOG"

log "build"
go build -o "$W/local-base" ./cmd/localbase || exit 5
go build -o "$W/fake-llama-server" ./internal/runtime/fakebackend || exit 5
go build -o "$W/embedding-measure" ./tools/embedding-measure || exit 5

if [ "$DRY" = 1 ]; then
  # A sparse file with just the header the planner reads, at the catalog's size.
  perl -e 'my ($path, $bytes) = @ARGV; open(my $f, ">:raw", $path) or die;
    sub s_ { pack("Q<", length $_[0]) . $_[0] }
    print $f pack("L<L<Q<Q<", 0x46554747, 3, 0, 3),
      s_("general.architecture"), pack("L<", 8), s_("qwen3"),
      s_("general.file_type"), pack("L<L<", 4, 7),
      s_("qwen3.context_length"), pack("L<L<", 4, 32768);
    close $f; truncate($path, $bytes) or die;' "$W/model.gguf" "$SIZE" || exit 5
  RT=$W/fake-llama-server
else
  log "clones, read-only from the live root"
  LIVE_ROOT=$HOME/.local/share/local-base
  # The live root can hold older runtimes. The one to use is the one this build pins.
  PINNED=""
  for d in "$LIVE_ROOT"/bin/runtimes/llama-server/*/; do
    name=$(basename "$d")
    if grep -qi "$name" internal/runtime/managed-runtime-manifest.json; then
      [ -z "$PINNED" ] || { log "more than one runtime is pinned: $PINNED $name"; exit 5; }
      PINNED=$name
    fi
  done
  [ -n "$PINNED" ] && [ -x "$LIVE_ROOT/bin/runtimes/llama-server/$PINNED/llama-server" ] || { log "the live root has no runtime this build pins"; exit 5; }
  cp -c -R "$LIVE_ROOT/bin/runtimes/llama-server/$PINNED" "$W/rt" || exit 5
  RT=$W/rt/llama-server
  cp -c "$LIVE_ROOT/models/llm/$FILE" "$W/model.gguf" || exit 5
  [ "$(stat -f %z "$W/model.gguf")" = "$SIZE" ] || { log "the model clone is not the catalog's size"; exit 5; }
  # With a time limit: a command substitution holds off the watcher's signal until it ends.
  log "runtime: $PINNED: $(perl -e 'alarm 10; exec @ARGV' "$RT" --version 2>&1 | grep -iE 'version|build' | head -2 | tr '\n' ' ' | cut -c1-120)"
fi

prepare_root() { # root
  "${SCRATCH_ENV[@]}" "$W/local-base" --root "$1" init >> "$W/init.log" 2>&1 || { log "init failed: $(tail -2 "$W/init.log" | cut -c1-200)"; exit 5; }
  "${SCRATCH_ENV[@]}" "$W/local-base" --root "$1" configure --parallel "$SLOTS" --ctx-size "$CTX" >> "$W/init.log" 2>&1 || { log "configure failed: $(tail -2 "$W/init.log" | cut -c1-200)"; exit 5; }
  mkdir -p "$1/models/llm"
  cp -c "$W/model.gguf" "$1/models/llm/$FILE" 2>/dev/null || cp "$W/model.gguf" "$1/models/llm/$FILE" || exit 5
}
start_gateway() { # root, llama-server, log
  # A plain command, so that $! is the gateway itself: stopping a wrapper would not stop it.
  "${SCRATCH_ENV[@]}" LOCALBASE_LLAMA_SERVER="$2" "$W/local-base" --root "$1" serve --no-auth --host 127.0.0.1 --port "$GW" --llm-port "$LLM" \
    --ctx-size "$CTX" --log-level debug --no-stt --no-tts --no-image --no-video >> "$3" 2>&1 &
  GWPID=$!
  for _ in $(seq 1 240); do
    [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$GW/health")" = 200 ] && break
    kill -0 "$GWPID" 2>/dev/null || { log "the scratch gateway exited: $(tail -3 "$3" | cut -c1-240)"; exit 5; }
    sleep 0.25
  done
  local enable
  enable=$(curl -s -m 60 -o "$W/enable.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"modelId\":\"$MODEL_ID\",\"action\":\"enable\"}" "http://127.0.0.1:$GW/_localbase/model-management")
  log "enable: $enable $(cut -c1-160 "$W/enable.json")"
}
starting() { grep -E '"eventName":"runtime\.starting"' "$1" | tail -1 | jq -c '.attributes | {demand_bytes, demand_confidence, ctx_size, slots}' 2>/dev/null; }
demand_mib() { grep -E '"eventName":"runtime\.starting"' "$1" | tail -1 | jq -r '(.attributes.demand_bytes // 0) / 1048576 | floor' 2>/dev/null; }
backend_of() { ps -A -o pid=,ppid=,command= | awk -v parent="$GWPID" -v rt="$RT" '$2 == parent && $3 == rt { print $1 }' | head -1; }
# The first request loads the model, which is when memory is most likely to spike. So it runs in the
# background with a wait: in a command substitution, the watcher's signal would wait for it, up to 180 s.
first_request() {
  curl -s -m 180 -o "$W/first.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL_ID\",\"input\":\"a first line\"}" "http://127.0.0.1:$GW/v1/embeddings" > "$W/first.status" &
  WORK=$!
  wait "$WORK"
  WORK=""
  status=$(cat "$W/first.status")
}

log "=== the planner's exact plan, with a fake backend in a separate root: nothing real is loaded"
prepare_root "$W/plan-root"
start_gateway "$W/plan-root" "$W/fake-llama-server" "$W/plan-gw.log"
first_request
log "plan request: $status: $(cut -c1-160 "$W/first.json" | head -1)"
PLAN=$(starting "$W/plan-gw.log")
PLAN_MIB=$(demand_mib "$W/plan-gw.log")
log "PLAN: $PLAN = ${PLAN_MIB:-unknown} MiB (limit $PLAN_LIMIT_MIB MiB)"
fake=$(backend_of)
[ -n "$fake" ] && log "PLAN args: $(ps -o command= -p "$fake" | sed "s|$W|<scratch>|g" | cut -c1-300)"
stop_gateway
sweep_left=$(ps -A -o pid=,command= | awk -v p="$W/fake-llama-server" 'index($2, p) == 1' | wc -l | tr -d ' ')
[ "$sweep_left" = 0 ] || { log "a fake backend outlived the plan's gateway"; exit 5; }
if [ -z "${PLAN_MIB:-}" ] || [ "${PLAN_MIB:-0}" -le 0 ]; then
  log "ABORT: no plan could be read, so nothing real is loaded"
  exit 6
fi
if [ "$PLAN_MIB" -gt "$PLAN_LIMIT_MIB" ]; then
  log "ABORT: the plan is $PLAN_MIB MiB, over the limit of $PLAN_LIMIT_MIB MiB. Nothing real was loaded."
  exit 6
fi

log "=== the pass: free memory $(free_pct)%, available $(pages_mib) MiB"
prepare_root "$W/root"
start_gateway "$W/root" "$RT" "$W/gw.log"
first_request
log "first request: $status: $(cut -c1-120 "$W/first.json" | head -1)"
[ "$status" = 200 ] || { log "ABORT: the model did not answer"; exit 7; }
BACKEND=$(backend_of)
[ -n "$BACKEND" ] || { log "ABORT: no backend of ours is running, so the answer did not come from it"; exit 7; }
BOUND=$(lsof -nP -a -p "$BACKEND" -iTCP -sTCP:LISTEN 2>/dev/null | awk 'NR > 1 { print $9 }' | tr '\n' ' ')
case "$BOUND" in
  "127.0.0.1:$LLM "*) ;;
  *) log "ABORT: backend $BACKEND listens on '$BOUND', not 127.0.0.1:$LLM"; exit 7 ;;
esac
REAL=$(demand_mib "$W/gw.log")
log "backend $BACKEND on $BOUND; the real start's plan: $(starting "$W/gw.log")"
log "backend args: $(ps -o command= -p "$BACKEND" | sed "s|$W|<scratch>|g" | cut -c1-300)"
if [ "${REAL:-0}" -gt "$PLAN_LIMIT_MIB" ]; then log "ABORT: the real start's plan is $REAL MiB, over the limit"; exit 6; fi

memory_now() { # label
  local rss foot
  rss=$(ps -o rss= -p "$BACKEND" | tr -d ' ')
  foot=$(perl -e 'alarm 20; exec @ARGV' /usr/bin/footprint -p "$BACKEND" 2>/dev/null | awk '/phys_footprint:/ { print $2 $3; exit }')
  log "MEMORY $1: rss ${rss:-?} KiB, footprint ${foot:-?}, available $(pages_mib) MiB, free $(free_pct)%"
}
memory_now "before measuring"

echo "time,rss_kib,available_mib" > "$CSV"
(
  trap - EXIT INT TERM HUP
  while :; do
    echo "$(date +%H:%M:%S),$(ps -o rss= -p "$BACKEND" | tr -d ' '),$(pages_mib)" >> "$CSV"
    sleep 2
  done
) &
SAMPLER=$!
disown "$SAMPLER"

# In the background with a wait, so that the watcher's signal stops it at once.
"$W/embedding-measure" -base "http://127.0.0.1:$GW" -model "$MODEL_ID" -out "$RESULTS" > >(tee -a "$EV") 2>&1 &
WORK=$!
wait "$WORK"
code=$?
WORK=""
if [ "$DRY" = 1 ] && [ -n "${DRY_HOLD_SECONDS:-}" ]; then
  # The fake answers at once. A rehearsal holds here, so that the stop paths have time to fire.
  sleep "$DRY_HOLD_SECONDS" &
  WORK=$!
  wait "$WORK"
  WORK=""
fi
kill "$SAMPLER" 2>/dev/null
SAMPLER=""
memory_now "after measuring"
log "peak rss: $(awk -F, 'NR > 1 && $2 != "" && $2 + 0 > m { m = $2 + 0 } END { print m + 0 }' "$CSV") KiB; lowest available: $(awk -F, 'NR > 1 && $3 != "" && (m == "" || $3 + 0 < m) { m = $3 + 0 } END { print m + 0 }' "$CSV") MiB"
log "backend still the same process: $([ "$(backend_of)" = "$BACKEND" ] && echo yes || echo NO)"
if [ "$code" != 0 ]; then log "ABORT: measuring failed, exit $code"; exit 7; fi
log "results: $RESULTS"
exit 0
