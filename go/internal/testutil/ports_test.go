package testutil

import "testing"

func TestPortRange(t *testing.T) {
	if l, h, err := PortRange(""); err != nil || l != 25000 || h != 25499 {
		t.Errorf("default: %d %d %v", l, h, err)
	}
	if l, h, err := PortRange("26000-26499"); err != nil || l != 26000 || h != 26499 {
		t.Errorf("set: %d %d %v", l, h, err)
	}
	for _, bad := range []string{"26000", "x-y", "26499-26000", "80-90", "60000-70000", "-"} {
		if _, _, err := PortRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFreePortStaysInBlockAndDoesNotRepeat(t *testing.T) {
	t.Setenv(PortsEnv, "25600-25620") // spans the excluded 25611-25613
	seen := map[int]bool{}
	for range 10 {
		p := FreePort()
		if !PortAllowed(p) || seen[p] {
			t.Errorf("port %d (seen %v)", p, seen)
		}
		seen[p] = true
	}
	t.Setenv(PortsEnv, "24173-24173")
	if PortAllowed(24173) {
		t.Error("24173 is never allowed")
	}
	defer func() {
		if recover() == nil {
			t.Error("a block with no usable port should panic")
		}
	}()
	FreePort()
}
