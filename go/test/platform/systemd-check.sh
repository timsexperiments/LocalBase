#!/bin/bash
# Opt-in Linux/systemd qualification (M6 step 3): runs the linux/arm64
# local-base binary as a systemd user service inside a disposable Debian
# container with systemd as PID 1, then removes the container and image.
#
#   tools/systemd-check.sh            # from go/, needs Docker and bun
#
# Inside the container, as a non-root user with lingering enabled, on a
# scratch root (so a scratch unit label): start, status, the installed unit
# against TS's renderer for the same manifest, a pending change that restart
# applies (config apply --restart never), the gateway's stderr in the
# journal, stop, and removal. No GPU, no published ports.
set -uo pipefail
cd "$(dirname "$0")/.."
tag="localbase-systemd-check:$$"
name="lbsc-$$"
work=$(mktemp -d "${TMPDIR:-/tmp}/lbsc-XXXXXX")
fails=0
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1
  docker rmi -f "$tag" >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
ok() { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

step "build the linux/arm64 binary and the image"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$work/local-base" ./cmd/localbase || exit 1
docker build --platform linux/arm64 -q -t "$tag" tools/systemd-check >/dev/null || exit 1
ok "built"

step "start systemd as PID 1"
docker run -d --name "$name" --platform linux/arm64 --privileged --cgroupns=private \
  --tmpfs /run --tmpfs /run/lock --tmpfs /tmp "$tag" >/dev/null || exit 1
for i in $(seq 1 60); do
  state=$(docker exec "$name" systemctl is-system-running --wait 2>&1)
  case "$state" in running|degraded) break ;; esac
  sleep 1
done
echo "system state: $state"
[ "$(docker exec "$name" cat /proc/1/comm)" = systemd ] && ok "PID 1 is systemd" || { bad "PID 1 is not systemd"; exit 1; }

step "user session with lingering"
for i in $(seq 1 60); do
  docker exec "$name" systemctl is-active --quiet systemd-logind.service && break
  sleep 1
done
docker exec "$name" loginctl enable-linger lb
docker exec "$name" systemctl start user@1000.service
for i in $(seq 1 90); do
  docker exec "$name" systemctl is-active --quiet user@1000.service && break
  sleep 1
done
docker exec "$name" systemctl is-active --quiet user@1000.service && ok "user@1000.service active" || { bad "no user manager"; docker exec "$name" systemctl status user@1000.service --no-pager 2>&1 | head -20; docker exec "$name" loginctl list-users 2>&1; exit 1; }
docker cp "$work/local-base" "$name:/usr/local/bin/local-base" >/dev/null
docker exec "$name" chmod 755 /usr/local/bin/local-base

as_lb() {
  docker exec -u lb -e HOME=/home/lb -e XDG_RUNTIME_DIR=/run/user/1000 \
    -e DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus -w /home/lb "$name" "$@"
}
root=/home/lb/lb-scratch-root
lb() { as_lb local-base --root "$root" "$@"; }
as_lb systemctl --user is-system-running --wait >/dev/null 2>&1
echo "user manager: $(as_lb systemctl --user is-system-running 2>&1)"

step "start"
lb init >/dev/null || bad "init"
lb start; rc=$?
[ $rc = 0 ] && ok "start exited 0" || bad "start exited $rc"
lb --json status > "$work/status1.json"; cat "$work/status1.json" | head -c 1500; echo
unit=$(as_lb sh -c 'ls ~/.config/systemd/user/local-base-gateway-*.service' 2>/dev/null)
[ -n "$unit" ] && ok "unit installed: $unit" || bad "no unit file"
uname=$(basename "$unit")
as_lb systemctl --user is-active --quiet "$uname" && ok "unit active" || bad "unit not active"
as_lb systemctl --user is-enabled --quiet "$uname" && ok "unit enabled" || bad "unit not enabled"
port=$(lb config show | sed -n 's/^port = \([0-9]*\)$/\1/p' | head -1)
health=""
for i in $(seq 1 30); do
  health=$(docker exec "$name" curl -s -m 2 "http://127.0.0.1:${port:-2273}/health") && [ -n "$health" ] && break
  sleep 1
done
[ -n "$health" ] && ok "gateway healthy on ${port:-2273}" || bad "gateway not answering /health"

step "the installed unit is what TS renders for the same manifest"
as_lb cat "$unit" > "$work/unit.go"
as_lb cat "$root/service/manifest.json" > "$work/manifest.json"
(cd .. && LOCALBASE_ROOT=/nonexistent bun run go/tools/golden/systemd-unit.ts < "$work/manifest.json") > "$work/unit.ts"
if cmp -s "$work/unit.go" "$work/unit.ts"; then ok "byte-identical to TS ($(wc -c < "$work/unit.go" | tr -d ' ') bytes)"; else bad "unit differs from TS"; diff "$work/unit.ts" "$work/unit.go"; fi
grep -q '^Environment="LOCALBASE_SERVICE_TOKEN=' "$work/unit.go" && grep -q '^StandardError=journal$' "$work/unit.go" && ok "token and journal stderr in unit" || bad "unit content"

step "a pending change is applied by restart"
lb config show > "$work/doc.toml"
newport=$((${port:-2273} + 1))
sed "s/^port = ${port:-2273}$/port = $newport/" "$work/doc.toml" > "$work/doc2.toml"
docker cp "$work/doc2.toml" "$name:/home/lb/doc2.toml" >/dev/null && docker exec "$name" chown lb /home/lb/doc2.toml
lb config apply --file /home/lb/doc2.toml --restart never; rc=$?
[ $rc = 0 ] && ok "config apply (restart never) exited 0" || bad "config apply exited $rc"
lb --json config show > "$work/pending.json"
pending=$(jq -c ".data.pendingRestart" "$work/pending.json")
[ "$pending" = true ] && ok "pending restart recorded: $pending" || bad "no pending restart recorded"
lb restart; rc=$?
[ $rc = 0 ] && ok "restart exited 0" || bad "restart exited $rc"
health=""
for i in $(seq 1 30); do
  health=$(docker exec "$name" curl -s -m 2 "http://127.0.0.1:$newport/health") && [ -n "$health" ] && break
  sleep 1
done
[ -n "$health" ] && ok "gateway now on $newport" || bad "gateway not on the new port"
for i in $(seq 1 15); do
  lb --json config show > "$work/after.json"
  [ "$(jq -c ".data.pendingRestart" "$work/after.json")" = false ] && break
  sleep 1
done
[ "$(jq -c ".data.pendingRestart" "$work/after.json")" = false ] && ok "pending change acknowledged" || bad "pending restart not cleared: $(jq -c ".data.pendingRestart" "$work/after.json")"

step "the gateway's stderr goes to the journal"
as_lb journalctl --user -u "$uname" --no-pager -o cat > "$work/journal.txt" 2>&1
lines=$(wc -l < "$work/journal.txt" | tr -d ' ')
echo "journal lines: $lines"; head -5 "$work/journal.txt"
grep -qi "listening\|gateway" "$work/journal.txt" && ok "gateway output in the journal" || bad "no gateway output in the journal"
as_lb test ! -e "$root/logs/service-stderr.log" && ok "no stderr file on Linux (journal only)" || bad "a stderr file exists on Linux"

step "stop and removal"
lb stop; rc=$?
[ $rc = 0 ] && ok "stop exited 0" || bad "stop exited $rc"
as_lb systemctl --user is-active --quiet "$uname" && bad "unit still active" || ok "unit inactive"
as_lb systemctl --user is-enabled --quiet "$uname" 2>/dev/null && bad "unit still enabled" || ok "unit disabled"
docker exec "$name" curl -s -m 2 "http://127.0.0.1:$newport/health" >/dev/null && bad "gateway still answering" || ok "gateway gone"
lb --json status > "$work/status2.json"; head -c 600 "$work/status2.json"; echo
lb uninstall --yes; echo "uninstall exit $?"
as_lb test -e "$unit" && bad "unit file left behind" || ok "unit file removed"
as_lb sh -c "systemctl --user list-unit-files 'local-base-gateway-*' --no-legend" | grep -q . && bad "unit still known to systemd" || ok "systemd no longer lists the unit"
as_lb test -e "$root" && bad "root left behind" || ok "root removed"

step "result"
if [ $fails = 0 ]; then echo "systemd check passed"; else echo "systemd check: $fails failure(s)"; fi
exit $((fails > 0))
