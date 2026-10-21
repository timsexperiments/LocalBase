#!/bin/bash
# M7 rehearsal, service steps (DESIGN §17.2), on a copy made by
# `tools/shadow.sh copy`: Go installed as a launchd service under the
# copy's own throwaway label, scratch ports, smoke, the client replay and
# the contract subset with the operator's temporary key, `config apply`
# with a launchd restart, the rollback drill as a binary swap under the
# same label (to the TS build), forward to Go again, and removal.
#
#   LBGO_TS_BIN=<TS binary> [LBGO_KEY_FILE=<key>] tools/rehearsal.sh
#
# LBGO_LIGHT=1 is the reduced drill for low headroom (operator, 2026-10-21):
# the copy's active LLM becomes LBGO_LIGHT_LLM (default the 1.5B coder) for
# the smokes, the replay sends no decision request, and the preflight needs
# 14 GiB instead of 25. The 12 GiB abort floor is unchanged.
#
# The key is read only into the clients' environment. Media is never
# requested: the TTS, image and video ports are flag-only and would be the
# live defaults.
set -uo pipefail
cd "$(dirname "$0")/.."
work=${LBGO_SHADOW:-/private/tmp/lbgo-shadow}
root="$work/root"
bin="$work/bin/local-base"
light=${LBGO_LIGHT:-}
light_llm=${LBGO_LIGHT_LLM:-qwen2.5-coder-1.5b-instruct-q4_k_m}
chat=${LBGO_CHAT:-}
decision=${LBGO_DECISION:-}
need_gib=25
if [ -n "$light" ]; then chat=$light_llm; decision=""; need_gib=14; fi
# The fit-aware default (DESIGN §18) set during the config apply step, when
# both models are selected on the copy.
defaults=${LBGO_DEFAULTS:-'"qwen3.6-35b-a3b-iq4_nl_xl","qwen3.5-9b-q4_k_m"'}
die() { echo "rehearsal: $*" >&2; exit 1; }
[ -d "$root" ] || die "no copy at $root (tools/shadow.sh copy first)"
[ -x "${LBGO_TS_BIN:-}" ] || die "set LBGO_TS_BIN to the TS build"
label="com.localbase.gateway.$(printf '%s' "$root" | shasum -a 256 | cut -d' ' -f1)"
plist="$HOME/Library/LaunchAgents/$label.plist"
live_label="com.localbase.gateway.$(printf '%s' "$HOME/.local/share/local-base" | shasum -a 256 | cut -d' ' -f1)"
[ "$label" != "$live_label" ] || die "the copy's label equals the live label"
fails=0
ok() { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
lb() { "$bin" --root "$root" "$@"; }
pick() { while :; do p=$((24000 + RANDOM % 6000)); [ $p = 24173 ] && continue; [ $p -ge 25611 ] && [ $p -le 25613 ] && continue
  lsof -nP -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1 || { echo $p; return; }; done; }
swap_in() { # swap_in <binary>: cp to .new, then mv over (the deploy rule)
  cp "$1" "$bin.new" && chmod 755 "$bin.new" && mv -f "$bin.new" "$bin"
}
smoke() { local t0=$(date +%s); out=$(lb smoke 2>&1); rc=$?; echo "$out" | tail -3; echo "smoke: $(( $(date +%s) - t0 )) s"; return $rc; }
health_port() { lb config show | sed -n '/^\[gateway\]/,/^\[/s/^port = \([0-9]*\)$/\1/p' | head -1; }
wait_health() { for i in $(seq 1 60); do curl -s -m 2 "http://127.0.0.1:$1/health" >/dev/null && return 0; sleep 1; done; return 1; }
cleanup() {
  [ -n "${watch_pid:-}" ] && kill "$watch_pid" 2>/dev/null
  [ -x "$bin" ] && [ -e "$root/.localbase-root.json" ] && lb stop >/dev/null 2>&1
  launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1
  rm -f "$plist"
}
trap cleanup EXIT
trap 'echo "rehearsal: aborted ($(cat "$work/abort" 2>/dev/null))"; exit 2' TERM INT

# Memory and the live gateway (operator conditions): at least 25 GiB
# available and the live gateway idle before starting; abort below 12 GiB
# or when live /health stops answering 200.
gib=1073741824
avail() { vm_stat | awk '/page size of/ {ps=$8} /Pages (free|inactive|speculative)/ {gsub("\\.","",$NF); s+=$NF} END {printf "%d\n", s*ps}'; }
live_health() { curl -s -o /dev/null -m 3 -w '%{http_code}' http://127.0.0.1:2273/health; }
live_idle() { # no inference.started without an end in the live event log's last 60 s (read-only)
  local since; since=$(date -u -v-60S +%Y-%m-%dT%H:%M:%S)
  tail -n 2000 "$HOME/.local/share/local-base/logs/events.jsonl" | awk -v s="$since" '
    match($0, /"timestamp":"[^"]*"/) { t=substr($0, RSTART+13, 19); if (t < s) next }
    /"eventName":"inference\.started"/ {st++} /"eventName":"inference\.(completed|failed|cancelled|aborted)"/ {en++}
    END {printf "started=%d finished=%d\n", st, en; exit !(st <= en)}'
}
preflight() {
  local a; a=$(avail)
  printf 'available %.1f GiB; ' "$(echo "$a / $gib" | bc -l)"
  [ "$a" -ge $((need_gib * gib)) ] || { echo; die "under $need_gib GiB available; wait"; }
  live_idle || die "the live gateway is not idle; wait"
  [ "$(live_health)" = 200 ] || die "live /health is not 200"
}
watch() { # background guard; records the lowest available memory and live /health
  local low=999999999999 a h probes=0 bad=0
  while :; do
    a=$(avail); [ "$a" -lt "$low" ] && low=$a
    h=$(live_health); probes=$((probes + 1)); [ "$h" = 200 ] || bad=$((bad + 1))
    printf 'lowest %.1f GiB, live health probes %d, non-200 %d\n' "$(echo "$low / $gib" | bc -l)" $probes $bad > "$work/watch.txt"
    if [ "$a" -lt $((12 * gib)) ]; then echo "available under 12 GiB" > "$work/abort"; kill -TERM $$; return; fi
    if [ "$h" != 200 ]; then echo "live /health answered $h" > "$work/abort"; kill -TERM $$; return; fi
    sleep 2
  done
}
snap() { # launchd snapshot: loaded labels and the LaunchAgents directory
  launchctl list | awk 'NR > 1 {print $3}' | sort > "$work/launchd-$1-labels.txt"
  ls -1 "$HOME/Library/LaunchAgents" | sort > "$work/launchd-$1-agents.txt"
}
in_scratch() { [ "$1" -ge 24000 ] && [ "$1" -le 29999 ] && [ "$1" != 24173 ] && { [ "$1" -lt 25611 ] || [ "$1" -gt 25613 ]; }; }
check_ports() { # every port the copy's processes listen on is in the scratch range, on loopback
  local pids p bad=0
  pids=$(pgrep -f "$root" | tr '\n' ',' | sed 's/,$//')
  [ -n "$pids" ] || { bad "no process of the copy found"; return 1; }
  while read -r addr; do
    p=${addr##*:}
    case "$addr" in 127.0.0.1:*|\[::1\]:*) ;; *) echo "not loopback: $addr"; bad=1 ;; esac
    in_scratch "$p" || { echo "outside the scratch range: $addr"; bad=1; }
  done < <(lsof -nP -a -p "$pids" -iTCP -sTCP:LISTEN 2>/dev/null | awk 'NR > 1 {print $9}')
  [ $bad = 0 ]
}

echo "throwaway label: $label"
echo "== preflight"
preflight
snap before
watch & watch_pid=$!
mkdir -m 700 -p "$work/bin"
go build -o "$work/local-base-go" ./cmd/localbase || die "build"
swap_in "$work/local-base-go"

echo "== scratch ports in the copy's config"
gport=$(pick); lport=$(pick); sport=$(pick)
lb config show > "$work/doc.toml"
awk -v g="$gport" -v l="$lport" -v s="$sport" '
  /^\[gateway\]/ {sec="g"} /^\[runtime\]/ {sec="r"} /^\[models\]/ {sec="m"}
  sec=="g" && /^host = / {print "host = \"127.0.0.1\""; next}
  sec=="g" && /^port = / {print "port = " g; next}
  sec=="r" && /^host = / {print "host = \"127.0.0.1\""; next}
  sec=="r" && /^port = / {print "port = " l; next}
  sec=="r" && /^sttHost = / {print "sttHost = \"127.0.0.1\""; next}
  sec=="r" && /^sttPort = / {print "sttPort = " s; next}
  sec=="m" && /^selected(Tts|Image|Video)Models = / {sub(/ = .*/, " = []"); print; next}
  sec=="m" && /^active(Tts|Image|Video)Model = / {sub(/ = .*/, " = \"\""); print; next}
  {print}' "$work/doc.toml" > "$work/doc-scratch.toml"
if [ -n "$light" ]; then # the 1.5B is the active LLM for the smokes
  grep -q "^selectedLlmModels = .*\"$light_llm\"" "$work/doc-scratch.toml" || die "$light_llm is not selected on the copy"
  sed -i '' "s/^activeLlmModel = .*/activeLlmModel = \"$light_llm\"/" "$work/doc-scratch.toml"
fi
lb config apply --file "$work/doc-scratch.toml" --restart never >/dev/null && ok "gateway $gport, llm $lport, stt $sport on 127.0.0.1" || die "config apply"
lb config show | grep -E '^(selected|active)(Tts|Image|Video)' | grep -vE '= (\[\]|"")$' && die "a media model is still selected" || ok "no media model selected (TTS, image and video runtimes cannot start)"

echo "== Go as a service under the throwaway label"
lb start >/dev/null; rc=$?
[ $rc = 0 ] && ok "start" || bad "start exited $rc"
wait_health "$gport" && ok "healthy on $gport" || bad "not healthy"
check_ports && ok "the copy listens only on loopback scratch ports" || die "the copy listens outside the scratch range"
snap during
[ -e "$plist" ] && ok "plist installed for the throwaway label" || bad "no plist at $plist"
plutil -extract ProgramArguments json -o - "$plist" | grep -q "\"$bin\"" && ok "plist runs $bin" || bad "plist program"
smoke && ok "Go smoke" || bad "Go smoke"

if [ -n "${LBGO_KEY_FILE:-}" ]; then
  echo "== client replay and contract subset with the temporary key"
  LOCALBASE_CONTRACT_KEY="$(cat "$LBGO_KEY_FILE")" LOCALBASE_REPLAY=1 LOCALBASE_REPLAY_CHAT="$chat" LOCALBASE_REPLAY_DECISION="$decision" \
    go test ./contract -count=1 -v -run '^(TestClientReplay|TestHealth|TestReadiness|TestAuthentication|TestUnmatchedRoute|TestModelRetrieve|TestChatRejections)$' \
    -args -url "http://127.0.0.1:$gport" 2>&1 | grep -E "replay_test.go|^--- |^ok|^FAIL" | tee "$work/clients.txt"
  grep -q "^ok" "$work/clients.txt" && ok "replay and contract subset" || bad "replay or contract subset"
fi

echo "== config apply with a launchd restart (and the default chat model list)"
gport2=$(pick)
sed "/^\[gateway\]/,/^\[/s/^port = $gport$/port = $gport2/" "$work/doc-scratch.toml" > "$work/doc-auto.toml"
with_defaults=""
all_selected() { # all_selected <doc>: every default is a selected LLM in it
  local id
  for id in $(echo "$defaults" | tr -d '"' | tr ',' ' '); do
    grep -q "^selectedLlmModels = .*\"$id\"" "$1" || return 1
  done
}
if [ -n "$defaults" ] && ! grep -q '^defaultLlmModels' "$work/doc-auto.toml" && all_selected "$work/doc-auto.toml"; then
  awk -v d="defaultLlmModels = [$defaults]" '{print} /^activeVideoModel = / {print d}' "$work/doc-auto.toml" > "$work/doc-auto.tmp" &&
    mv "$work/doc-auto.tmp" "$work/doc-auto.toml"
  with_defaults=1
else
  echo "skipping defaultLlmModels: [$defaults] are not all selected on the copy"
fi
out=$(lb --json config apply --file "$work/doc-auto.toml" --wait 2>&1)
echo "$out" | jq -c '.data | {activation: (.activation // .result.activation), pendingRestart}' 2>/dev/null
echo "$out" | grep -q '"restarted"' && ok "--restart auto restarted the service" || bad "no restart: $(echo "$out" | head -c 300)"
wait_health "$gport2" && ok "healthy on the new port $gport2" || bad "not on $gport2"
[ "$(lb --json config show | jq -c .data.pendingRestart)" = false ] && ok "--wait returned with the change acknowledged" || bad "pending after --wait"
if [ -n "$with_defaults" ]; then
  lb config show | grep -q "^defaultLlmModels = \[$defaults\]" && ok "defaultLlmModels applied" || bad "defaultLlmModels not applied"
  line=$(lb status 2>&1 | grep "^Default chat model:")
  [ -n "$line" ] && ok "status: $line" || bad "status shows no default chat model"
fi
gport3=$(pick)
sed "/^\[gateway\]/,/^\[/s/^port = $gport$/port = $gport3/" "$work/doc-scratch.toml" > "$work/doc-never.toml"
lb config apply --file "$work/doc-never.toml" --restart never >/dev/null
[ "$(lb --json config show | jq -c .data.pendingRestart)" = true ] && ok "--restart never records a pending restart" || bad "no pending restart"
lb restart >/dev/null
wait_health "$gport3" && ok "restart applied it (port $gport3)" || bad "restart did not apply"
for i in $(seq 1 15); do [ "$(lb --json config show | jq -c .data.pendingRestart)" = false ] && break; sleep 1; done
[ "$(lb --json config show | jq -c .data.pendingRestart)" = false ] && ok "pending restart cleared" || bad "pending restart not cleared"

echo "== rollback drill: binary swap to TS under the same label"
t0=$(date +%s)
lb stop >/dev/null
mv -f "$bin" "$work/local-base.go-aside"
swap_in "$LBGO_TS_BIN"
echo "version: $(lb --version 2>&1 | head -1)"
lb restart >/dev/null; rc=$?
[ $rc = 0 ] && ok "TS restart" || bad "TS restart exited $rc"
wait_health "$gport3" || bad "TS not healthy"
smoke && ok "TS smoke" || bad "TS smoke"
echo "rollback time to smoke: $(( $(date +%s) - t0 )) s" | tee "$work/rollback-time.txt"
plutil -extract ProgramArguments json -o - "$plist" | grep -q "\"$bin\"" && ok "TS rewrote the plist under the same label and path" || bad "plist after TS"
if [ -n "${LBGO_KEY_FILE:-}" ]; then
  LOCALBASE_CONTRACT_KEY="$(cat "$LBGO_KEY_FILE")" LOCALBASE_REPLAY=1 LOCALBASE_REPLAY_CHAT="$chat" LOCALBASE_REPLAY_DECISION="$decision" \
    go test ./contract -count=1 -run '^TestClientReplay$' -args -url "http://127.0.0.1:$gport3" >/dev/null 2>&1 && ok "replay on TS" || bad "replay on TS"
fi

echo "== forward to Go again"
t0=$(date +%s)
lb stop >/dev/null
swap_in "$work/local-base.go-aside"
lb restart >/dev/null && wait_health "$gport3" && smoke && ok "Go again: smoke in $(( $(date +%s) - t0 )) s" || bad "Go again"

echo "== removal"
lb stop >/dev/null && ok "stopped"
lb uninstall --yes >/dev/null; echo "uninstall exit $?"
[ -e "$plist" ] && bad "plist left behind" || ok "plist removed"
launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 && bad "label still loaded" || ok "label not loaded"
[ -e "$root" ] && bad "copy root left behind" || ok "copy root removed by uninstall"
launchctl print "gui/$(id -u)/$live_label" >/dev/null 2>&1 && ok "the live label is still loaded" || bad "the live label is not loaded"
snap after
echo "launchd during the drill (before -> during):"; diff "$work/launchd-before-labels.txt" "$work/launchd-during-labels.txt"; diff "$work/launchd-before-agents.txt" "$work/launchd-during-agents.txt"
during=$( (diff "$work/launchd-before-labels.txt" "$work/launchd-during-labels.txt"; diff "$work/launchd-before-agents.txt" "$work/launchd-during-agents.txt") | grep '^[<>]' | grep -vE "^> ${label}(\.plist)?$")
[ -z "$during" ] && ok "during: only the throwaway label and plist were added" || bad "unexpected launchd change: $during"
if diff -q "$work/launchd-before-labels.txt" "$work/launchd-after-labels.txt" >/dev/null && diff -q "$work/launchd-before-agents.txt" "$work/launchd-after-agents.txt" >/dev/null; then
  ok "after: labels and LaunchAgents identical to before"
else
  bad "launchd differs after the drill"; diff "$work/launchd-before-labels.txt" "$work/launchd-after-labels.txt"; diff "$work/launchd-before-agents.txt" "$work/launchd-after-agents.txt"
fi
kill "$watch_pid" 2>/dev/null; echo "guard: $(cat "$work/watch.txt" 2>/dev/null)"

if [ $fails = 0 ]; then echo "rehearsal passed"; else echo "rehearsal: $fails failure(s)"; fi
exit $((fails > 0))
