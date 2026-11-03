#!/bin/bash
# tools/upgrade-rehearsal under real systemd: a disposable Debian with systemd as PID 1, the
# same image as tools/systemd-check.sh, then the container and image are removed. No published
# ports, no GPU, no network use by the drill.
#
#   tools/upgrade-rehearsal-systemd.sh       # from go/, needs Docker
#
# LBUR_IMAGE=<image> uses an image already built, and leaves it there.
set -uo pipefail
cd "$(dirname "$0")/.."
tag=${LBUR_IMAGE:-localbase-upgrade-rehearsal:$$}
name="lbur-$$"
work=$(mktemp -d "${TMPDIR:-/tmp}/lbur-XXXXXX")
arch=$(docker info --format '{{.Architecture}}')
case "$arch" in aarch64|arm64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) echo "unsupported: $arch" >&2; exit 1 ;; esac
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1
  [ -n "${LBUR_IMAGE:-}" ] || docker rmi -f "$tag" >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

go run ./tools/upgrade-rehearsal --build-kit "$work/kit" --goos linux --goarch "$arch" || exit 1
GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o "$work/upgrade-rehearsal" ./tools/upgrade-rehearsal || exit 1
[ -n "${LBUR_IMAGE:-}" ] || docker build --platform "linux/$arch" -q -t "$tag" tools/systemd-check >/dev/null || exit 1
docker run -d --name "$name" --platform "linux/$arch" --privileged --cgroupns=private \
  --tmpfs /run --tmpfs /run/lock --tmpfs /tmp "$tag" >/dev/null || exit 1
for _ in $(seq 1 60); do
  state=$(docker exec "$name" timeout 120 systemctl is-system-running --wait 2>&1)
  case "$state" in running|degraded) break ;; esac
  sleep 1
done
[ "$(docker exec "$name" cat /proc/1/comm)" = systemd ] || { echo "PID 1 is not systemd" >&2; exit 1; }
for _ in $(seq 1 60); do
  docker exec "$name" systemctl is-active --quiet systemd-logind.service && break
  sleep 1
done
docker exec "$name" loginctl enable-linger lb
docker exec "$name" timeout 120 systemctl start user@1000.service
for _ in $(seq 1 90); do
  docker exec "$name" systemctl is-active --quiet user@1000.service && break
  sleep 1
done
docker cp "$work/kit" "$name:/home/lb/kit" >/dev/null || exit 1
docker cp "$work/upgrade-rehearsal" "$name:/usr/local/bin/upgrade-rehearsal" >/dev/null || exit 1
docker exec "$name" chown -R lb /home/lb/kit
docker exec "$name" chmod 755 /usr/local/bin/upgrade-rehearsal
docker exec -u lb -e HOME=/home/lb -e XDG_RUNTIME_DIR=/run/user/1000 \
  -e DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus -w /home/lb "$name" \
  upgrade-rehearsal --work /home/lb/lb-upgrade-rehearsal --kit /home/lb/kit
