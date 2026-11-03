#!/bin/bash
# Does a real backend keep using more memory with every request? One bounded pass on a
# real model, in a window the operator has granted:
#   LOCALBASE_REAL_WINDOW=go tools/real-memory-check.sh <scratch dir>
# Rehearsal, with the fake backend and a stub for live /health, touching nothing live:
#   tools/real-memory-check.sh --dry-run <scratch dir>
#
# THE HYPOTHESIS IS UNSUPPORTED UNTIL THE RSS CURVE SHOWS IT. The gateway always passes
# --cache-ram 2048, so a prompt cache filling at about 100 MiB a request would stop near
# +2 GiB. A plateau near +2 GiB and then flat supports that. A straight line past it is a
# leak. What prompted this was system-wide available memory on a Linux runner, not the
# backend, so a result here is about this Mac and Metal: Linux CPU may differ. It is also about
# this model, a 1.5B coder model: growth per request may scale differently on other models.
#
# DOCUMENTED EXCEPTION TO "NEVER THE LIVE PORT". Inside a granted window only, the watcher
# may GET http://127.0.0.1:2273/health every 3 s, and nothing else on the live port: no other
# path, no POST, no live llama-server port. It stops everything if the live modalities state
# changes or free memory falls under the floor. Before the window, use vm_stat or
# memory_pressure only, and the operator does any live check.
#
# THE ONLY OTHER TOUCH OF THE LIVE ROOT is one read-only APFS clone, inside the window, of
# exactly these two paths and nothing else (no database, keys, config, ui-access.json or logs),
# and nothing runs from the live root:
#   $HOME/.local/share/local-base/models/llm/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf
#   $HOME/.local/share/local-base/bin/runtimes/llama-server/0f9c093f77b9c194f2f097df6204d9f5f8758968815e7db0374e98b0485ef9df
# If either is missing the pass stops, having loaded nothing. Both are logged.
#
# Limits the operator set: qwen2.5-coder-1.5b-instruct-q4_k_m, context 8192, one slot, default reserve, ports
# in 29000-29499. The gateway planner's exact plan is printed BEFORE any real load, from a
# separate scratch root with a fake backend (so no peak recorded there can skew the real
# root), and the pass aborts if it is over the limit below. At least 48% free to start, 40% floor
# throughout (free + speculative + inactive pages). 200 sequential requests, short outputs.
# One pass only: a second is a new ask of the operator.
#
# This script deletes nothing. It prints the paths to delete afterwards.
set -u
case "${1:-}" in
  --dry-run) DRY=1; shift ;;
  *) DRY=0 ;;
esac
if [ $# -ne 1 ]; then
  echo "usage: LOCALBASE_REAL_WINDOW=go tools/real-memory-check.sh <scratch dir>, or --dry-run <scratch dir>" >&2
  exit 2
fi
if [ "$DRY" = 0 ] && [ "${LOCALBASE_REAL_WINDOW:-}" != go ]; then
  echo "real-memory-check: only in a window the operator has granted: set LOCALBASE_REAL_WINDOW=go" >&2
  exit 2
fi
cd "$(dirname "$0")/.." || exit 2
SRC=$PWD
mkdir -p "$1" || exit 2
W=$(cd "$1" && pwd)/real-memory
if [ -e "$W" ]; then
  echo "real-memory-check: $W already exists; delete it first" >&2
  exit 2
fi
mkdir -p "$W" || exit 2
EV=$W/evidence.txt
CSV=$W/requests.csv
: > "$EV"

MODEL_ID=qwen2.5-coder-1.5b-instruct-q4_k_m
# A former catalog entry: an install with its file on disk adopts it, as the live root has.
SPEC=internal/catalog/data/former/llm/$MODEL_ID.json
GW=29410 LLM=29411
CTX=8192
PLAN_LIMIT_MIB=4864 # 4.75 GiB
START_PCT=48
FLOOR_PCT=40
REQUESTS=200
HARD_SECONDS=1500
LIVE=http://127.0.0.1:2273/health
LIVE_ROOT=$HOME/.local/share/local-base
if [ "$DRY" = 1 ]; then
  LIVE_ROOT=$W/fake-live-root # the same clone below, from a copy of the layout
  # A rehearsal's own stub takes the place of live /health. These can only change it.
  LIVE=http://127.0.0.1:29412/health
  START_PCT=${DRY_START_PCT:-0}
  FLOOR_PCT=${DRY_FLOOR_PCT:-0}
  REQUESTS=${DRY_REQUESTS:-30}
  PLAN_LIMIT_MIB=${DRY_PLAN_LIMIT_MIB:-$PLAN_LIMIT_MIB}
fi

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$EV"; }
pages_mib() {
  vm_stat | awk '/page size of/ { page = $8 } /Pages (free|speculative|inactive)/ { gsub(/\./, "", $NF); n += $NF } END { printf "%d\n", n * page / 1048576 }'
}
free_pct() { echo $(( $(pages_mib) * 1048576 * 100 / $(sysctl -n hw.memsize) )); }
live_state() { curl -s -m 3 "$LIVE" | jq -c '.modalities // "unreachable"' 2>/dev/null || echo unreachable; }
SCRATCH_ENV=(env -i PATH=/usr/bin:/bin:/usr/sbin "HOME=$W/home" LOCALBASE_CATALOG_FEED_URL=off
  HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 NO_PROXY=127.0.0.1,localhost)
# A rehearsal can make the fake slow to load, to test a stop during a load.
[ "$DRY" = 1 ] && [ -n "${DRY_FAKE_STARTUP_MS:-}" ] && SCRATCH_ENV+=("FAKE_STARTUP_MS=$DRY_FAKE_STARTUP_MS")

GWPID="" STUB="" MON="" DOG=""
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
# The backend alone, at once: during a load the gateway can take many seconds to go, and the
# model would go on loading all that time. The gateway stays, to be stopped properly next.
kill_backend() {
  ps -A -o pid=,command= | awk -v a="$W/rt/" -v b="$W/fake-llama-server" 'index($2, a) == 1 || index($2, b) == 1 { print $1 }' | while read -r pid; do
    kill -KILL "$pid" 2>/dev/null
  done
}
finish() {
  local code=$1
  # Ignored, not reset: a signal arriving now must not stop the cleanup and leave a model loaded.
  trap '' INT TERM HUP
  trap - EXIT
  [ -n "$MON" ] && kill "$MON" 2>/dev/null
  [ -n "$DOG" ] && kill "$DOG" 2>/dev/null
  [ "$code" = 0 ] || kill_backend
  stop_gateway
  sweep
  [ -n "$STUB" ] && kill "$STUB" 2>/dev/null
  log "END: exit $code; running from the scratch path: $(ps -A -o pid=,command= | awk -v p="$W/" 'index($2, p) == 1' | wc -l | tr -d ' ')"
  log "END: free memory $(free_pct)%"
  log "END: nothing was deleted. When done, delete by exact path: $W"
  exit "$code"
}
# A signal landing as bash forks a subshell can run this in the child too. Only the script itself
# cleans up, so that the record shows one stop. (No BASHPID in macOS's bash 3.2: sh names its parent.)
trap 'trap "" INT TERM HUP; [ "$(sh -c "echo \$PPID")" = "$$" ] || exit 3; log "stopped by a signal"; finish 3' INT TERM HUP
trap 'finish $?' EXIT

# A stale listener would answer in place of ours, and a run could look fine while measuring
# nothing. Bash's own /dev/tcp, so this needs nothing installed.
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
refuse_busy_ports() {
  local p
  for p in "$@"; do
    if port_busy "$p"; then
      log "ABORT: port $p is already in use, so a listener that is not this run's could answer. Nothing was started."
      exit 4
    fi
  done
}

log "plan: $MODEL_ID, context $CTX, one slot, $REQUESTS requests; limit $PLAN_LIMIT_MIB MiB; start at $START_PCT% free, floor $FLOOR_PCT%; dry run $DRY"

refuse_busy_ports "$GW" "$LLM"
[ "$DRY" = 1 ] && refuse_busy_ports 29412

FILE=$(jq -r '.artifacts[] | select(.role == "primary") | .filename' "$SPEC")
[ -n "$FILE" ] || { log "no primary file for $MODEL_ID in the catalog"; exit 5; }
SIZE=$(jq -r '.artifacts[] | select(.role == "primary") | .expectedSizeBytes' "$SPEC")

if [ "$DRY" = 1 ]; then
  mkdir -p "$W/stub"
  echo '{"modalities":{"llm":{"configured":true,"state":"idle"}}}' > "$W/stub/health"
  (cd "$W/stub" && exec /usr/bin/python3 -I -m http.server 29412 --bind 127.0.0.1 > /dev/null 2>&1) &
  STUB=$!
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
( trap - EXIT INT TERM HUP; sleep "$HARD_SECONDS"; echo "[$(date +%H:%M:%S)] WATCHDOG after $HARD_SECONDS s" >> "$EV"; kill -TERM $$ ) &
DOG=$!

log "build"
go build -o "$W/local-base" ./cmd/localbase || exit 5
go build -o "$W/fake-llama-server" ./internal/runtime/fakebackend || exit 5

SOURCE_MODEL=$LIVE_ROOT/models/llm/$FILE
# The live root holds more than one runtime directory: the one to use is what this build pins.
RUNTIME_ID=0f9c093f77b9c194f2f097df6204d9f5f8758968815e7db0374e98b0485ef9df
if ! grep -q "$RUNTIME_ID" internal/runtime/managed-runtime-manifest.json; then
  log "ABORT: this build no longer pins runtime $RUNTIME_ID. Update this script."
  exit 5
fi
SOURCE_RUNTIME=$LIVE_ROOT/bin/runtimes/llama-server/$RUNTIME_ID
if [ "$DRY" = 1 ]; then
  # A sparse file with just the header the planner reads, at the catalog's size.
  mkdir -p "$SOURCE_RUNTIME" "$(dirname "$SOURCE_MODEL")" || exit 5
  cp "$W/fake-llama-server" "$SOURCE_RUNTIME/llama-server" || exit 5
  [ "${DRY_DROP_MODEL:-}" = 1 ] || perl -e 'my ($path, $bytes) = @ARGV; open(my $f, ">:raw", $path) or die;
    sub s_ { pack("Q<", length $_[0]) . $_[0] }
    print $f pack("L<L<Q<Q<", 0x46554747, 3, 0, 3),
      s_("general.architecture"), pack("L<", 8), s_("qwen2"),
      s_("general.file_type"), pack("L<L<", 4, 15),
      s_("qwen2.context_length"), pack("L<L<", 4, 32768);
    close $f; truncate($path, $bytes) or die;' "$SOURCE_MODEL" "$SIZE" || exit 5
fi
log "clone source, read-only: $SOURCE_MODEL"
log "clone source, read-only: $SOURCE_RUNTIME"
if [ ! -f "$SOURCE_MODEL" ] || [ ! -x "$SOURCE_RUNTIME/llama-server" ]; then
  log "ABORT: a clone source is missing. Nothing was loaded, and nothing else was read."
  exit 5
fi
cp -c -R "$SOURCE_RUNTIME" "$W/rt" || exit 5
RT=$W/rt/llama-server
cp -c "$SOURCE_MODEL" "$W/model.gguf" || exit 5
# Bounded: a binary that does not exit on --version must not hang the window. The fake serves
# instead of exiting, and on a port this script does not choose, so a rehearsal tries the bound
# on something that hangs.
version_of() { # binary, quarter seconds
  "$1" --version > "$W/version.txt" 2>&1 &
  local v=$!
  for _ in $(seq 1 "$2"); do kill -0 "$v" 2>/dev/null || break; sleep 0.25; done
  kill -KILL "$v" 2>/dev/null
  wait "$v" 2>/dev/null
  grep -iE 'version|build' "$W/version.txt" | head -2 | tr '\n' ' ' | cut -c1-120
}
if [ "$DRY" = 1 ]; then
  printf '#!/bin/sh\nexec sleep 60\n' > "$W/hang"
  chmod +x "$W/hang"
  version_of "$W/hang" 4 > /dev/null
  log "runtime: a rehearsal; the bound on --version held"
else
  log "runtime: $(version_of "$RT" 80)"
fi

prepare_root() { # root
  "${SCRATCH_ENV[@]}" "$W/local-base" --root "$1" init >> "$W/init.log" 2>&1 || { log "init failed: $(tail -2 "$W/init.log" | cut -c1-200)"; exit 5; }
  "${SCRATCH_ENV[@]}" "$W/local-base" --root "$1" configure --parallel 1 --ctx-size "$CTX" >> "$W/init.log" 2>&1 || { log "configure failed: $(tail -2 "$W/init.log" | cut -c1-200)"; exit 5; }
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
ask() { # n; writes the body to $W/body.json, prints "status ms"
  local n=$1 began ended status
  # About 1,500 tokens, different from the first word, so that each is a new prompt to the cache.
  awk -v n="$n" 'BEGIN { srand(n); printf "Request %d. Repeat the last word only.", n; for (i = 0; i < 1000; i++) printf " %c%c%c%c", 97 + rand() * 26, 97 + rand() * 26, 97 + rand() * 26, 97 + rand() * 26 }' > "$W/prompt.txt"
  jq -n --arg m "$MODEL_ID" --rawfile p "$W/prompt.txt" '{model: $m, stream: false, max_tokens: 8, temperature: 0, messages: [{role: "user", content: $p}]}' > "$W/req.json"
  began=$(perl -MTime::HiRes=time -e 'printf "%d", time * 1000')
  status=$(curl -s -m 180 -o "$W/body.json" -w '%{http_code}' -H 'Content-Type: application/json' -d @"$W/req.json" "http://127.0.0.1:$GW/v1/chat/completions")
  ended=$(perl -MTime::HiRes=time -e 'printf "%d", time * 1000')
  echo "$status $((ended - began))"
}
starting() { grep -E '"eventName":"runtime\.starting"' "$1" | tail -1 | jq -c '{demand_bytes: .attributes.demand_bytes, confidence: .attributes.demand_confidence, ctx: (.attributes.ctx_size // .attributes.ctx)}' 2>/dev/null; }
demand_mib() { grep -E '"eventName":"runtime\.starting"' "$1" | tail -1 | jq -r '(.attributes.demand_bytes // 0) / 1048576 | floor' 2>/dev/null; }
backend_of() { ps -A -o pid=,ppid=,command= | awk -v parent="$GWPID" -v rt="$RT" '$2 == parent && $3 == rt { print $1 }' | head -1; }

log "=== the planner's exact plan, with a fake backend in a separate root: nothing real is loaded"
prepare_root "$W/plan-root"
start_gateway "$W/plan-root" "$W/fake-llama-server" "$W/plan-gw.log"
read -r status ms < <(ask 0)
log "plan request: $status in $ms ms: $(cut -c1-160 "$W/body.json" | head -1)"
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
echo "request,status,ms,prompt_tokens,rss_kib,footprint,available_mib" > "$CSV"
bad=0
BACKEND=""
n=0
while [ "$n" -lt "$REQUESTS" ]; do
  n=$((n + 1))
  read -r status ms < <(ask "$n")
  if [ "$status" = 200 ]; then bad=0; else
    bad=$((bad + 1))
    log "request $n: $status: $(cut -c1-200 "$W/body.json" | head -1)"
    if [ "$bad" -ge 5 ]; then log "ABORT: five failures in a row"; exit 7; fi
  fi
  if [ -z "$BACKEND" ]; then
    BACKEND=$(backend_of)
    if [ -n "$BACKEND" ]; then
      REAL=$(demand_mib "$W/gw.log")
      log "backend $BACKEND; the real start's plan: $(starting "$W/gw.log")"
      log "backend args: $(ps -o command= -p "$BACKEND" | sed "s|$W|<scratch>|g" | cut -c1-300)"
      if [ "${REAL:-0}" -gt "$PLAN_LIMIT_MIB" ]; then log "ABORT: the real start's plan is $REAL MiB, over the limit"; exit 6; fi
    fi
  fi
  rss=""; foot=""
  if [ -n "$BACKEND" ]; then
    rss=$(ps -o rss= -p "$BACKEND" | tr -d ' ')
    if [ $((n % 10)) = 1 ]; then
      foot=$(/usr/bin/footprint -p "$BACKEND" 2>/dev/null | awk '/phys_footprint:/ { print $2 $3; exit }')
    fi
  fi
  echo "$n,$status,$ms,$(jq -r '.usage.prompt_tokens // ""' "$W/body.json" 2>/dev/null),$rss,$foot,$(pages_mib)" >> "$CSV"
  if [ $((n % 25)) = 0 ]; then log "request $n: rss ${rss:-?} KiB, available $(pages_mib) MiB, free $(free_pct)%, last $ms ms"; fi
done

log "=== the curve, from the backend's RSS"
awk -F, -v limit=2048 'NR > 1 && $5 != "" { n++; r[n] = $5 / 1024; a[n] = $7; req[n] = $1 }
  END {
    if (n < 20) { print "too few samples to say anything"; exit }
    printf "RSS MiB at request:"; for (i = 1; i <= n; i++) if (req[i] == 1 || req[i] % 25 == 0) printf " %d=%.0f", req[i], r[i]; printf "\n"
    printf "available MiB at request:"; for (i = 1; i <= n; i++) if (req[i] == 1 || req[i] % 25 == 0) printf " %d=%d", req[i], a[i]; printf "\n"
    half = int(n / 2); m = 0; sx = 0; sy = 0; sxx = 0; sxy = 0
    for (i = half + 1; i <= n; i++) { m++; sx += i; sy += r[i]; sxx += i * i; sxy += i * r[i] }
    slope = (m * sxy - sx * sy) / (m * sxx - sx * sx)
    rise = r[n] - r[1]
    printf "RSS rose %.0f MiB in all; over the second half it moved %.2f MiB a request\n", rise, slope
    if (slope < 0.5 && rise > limit * 0.5 && rise < limit * 1.5) print "READING: a plateau near +2 GiB and then flat. This supports the cache hypothesis, on this Mac."
    else if (slope < 0.5) print "READING: flat in the second half, but not near +2 GiB. This does not support the cache hypothesis as stated, and does not show a leak here."
    else if (rise > limit * 1.25) print "READING: still rising, and past +2 GiB. This looks like a leak."
    else print "READING: still rising, and below +2 GiB. 200 requests were not enough to say."
  }' "$CSV" | tee -a "$EV"
log "the backend's own word on its cache, last lines: $(grep -oE 'cache state: [^"]*' "$W/gw.log" | tail -2 | tr '\n' ';' | cut -c1-240)"
log "This was $MODEL_ID, a 1.5B coder model: growth per request may scale differently on other models."
log "Mac and Metal only: Linux CPU may differ, so this does not settle what the Linux runner showed."
exit 0
