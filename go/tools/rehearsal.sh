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
# The key is read only into the clients' environment. Media is never
# requested: the TTS, image and video ports are flag-only and would be the
# live defaults.
set -uo pipefail
cd "$(dirname "$0")/.."
work=${LBGO_SHADOW:-/private/tmp/lbgo-shadow}
root="$work/root"
bin="$work/bin/local-base"
chat=${LBGO_CHAT:-}
decision=${LBGO_DECISION:-}
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
  [ -x "$bin" ] && [ -e "$root/.localbase-root.json" ] && lb stop >/dev/null 2>&1
  launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1
  rm -f "$plist"
}
trap cleanup EXIT

echo "throwaway label: $label"
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
  {print}' "$work/doc.toml" > "$work/doc-scratch.toml"
lb config apply --file "$work/doc-scratch.toml" --restart never >/dev/null && ok "gateway $gport, llm $lport, stt $sport on 127.0.0.1" || die "config apply"

echo "== Go as a service under the throwaway label"
lb start >/dev/null; rc=$?
[ $rc = 0 ] && ok "start" || bad "start exited $rc"
wait_health "$gport" && ok "healthy on $gport" || bad "not healthy"
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

echo "== config apply with a launchd restart"
gport2=$(pick)
sed "/^\[gateway\]/,/^\[/s/^port = $gport$/port = $gport2/" "$work/doc-scratch.toml" > "$work/doc-auto.toml"
out=$(lb --json config apply --file "$work/doc-auto.toml" --wait 2>&1)
echo "$out" | jq -c '.data | {activation: (.activation // .result.activation), pendingRestart}' 2>/dev/null
echo "$out" | grep -q '"restarted"' && ok "--restart auto restarted the service" || bad "no restart: $(echo "$out" | head -c 300)"
wait_health "$gport2" && ok "healthy on the new port $gport2" || bad "not on $gport2"
[ "$(lb --json config show | jq -c .data.pendingRestart)" = false ] && ok "--wait returned with the change acknowledged" || bad "pending after --wait"
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

if [ $fails = 0 ]; then echo "rehearsal passed"; else echo "rehearsal: $fails failure(s)"; fi
exit $((fails > 0))
