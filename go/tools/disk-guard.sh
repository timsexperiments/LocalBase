# Sourced by bash scripts that can write large files (see tools/README.md). disk_guard refuses
# to go on under a free-disk floor, and caps each file this shell and its children write.
disk_guard() {
  local who=${1:-this script} default=90
  [ -n "${CI:-}" ] && default=5
  local floor=${LOCALBASE_DISK_FLOOR_GB:-$default} cap=${LOCALBASE_FILE_CAP_GB:-64} free
  free=$(($(df -Pk "${TMPDIR:-/tmp}" | awk 'NR==2 {print $4}') / 1024 / 1024))
  if [ "$free" -lt "$floor" ]; then
    echo "$who: only $free GB free on the temporary volume, and the floor is $floor GB; refusing to start" >&2
    exit 3
  fi
  ulimit -f $((cap * 1024 * 1024)) # bash counts in 1024-byte blocks
}
