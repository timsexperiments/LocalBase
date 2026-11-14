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

# test_port_pick prints a free port in the block. It starts at a random port and walks the block
# once, so each port is tried at most once. With none free, or none it may use, it says so and
# fails, rather than waiting for one to come free.
test_port_pick() {
  local p n start k
  n=$((_test_port_high - _test_port_low + 1))
  start=$(($(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % n))
  k=0
  while [ "$k" -lt "$n" ]; do
    p=$((_test_port_low + (start + k) % n))
    k=$((k + 1))
    test_port_ok "$p" || continue
    lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 || { echo "$p"; return 0; }
  done
  echo "test_port_pick: no free port in $_test_port_low-$_test_port_high" >&2
  return 1
}
