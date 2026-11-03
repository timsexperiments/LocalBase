# Sourced by the check scripts: scratch ports from one block, as the Go tests
# take them (internal/testutil/ports.go). LOCALBASE_TEST_PORTS=low-high chooses
# the block. 24173 and 25611-25613 are never used.
_test_ports=${LOCALBASE_TEST_PORTS:-25000-25499}
_test_port_low=${_test_ports%-*}
_test_port_high=${_test_ports#*-}

test_port_ok() {
  [ "$1" -ge "$_test_port_low" ] && [ "$1" -le "$_test_port_high" ] && [ "$1" != 24173 ] &&
    { [ "$1" -lt 25611 ] || [ "$1" -gt 25613 ]; }
}

test_port_pick() {
  local p
  while :; do
    p=$((_test_port_low + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % (_test_port_high - _test_port_low + 1)))
    test_port_ok "$p" || continue
    lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 || { echo "$p"; return; }
  done
}
