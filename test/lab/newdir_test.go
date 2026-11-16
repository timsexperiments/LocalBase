package lab

import (
	"os"
	"path/filepath"
	"testing"
)

// Each new gateway gets a directory no gateway has used: a name used again gets -2, -3 and so on.
func TestNewDirNeverReusesADirectory(t *testing.T) {
	parent := t.TempDir()
	for _, want := range []string{"gw", "gw-2", "gw-3"} {
		got, err := newDir(parent, "gw")
		if err != nil || got != filepath.Join(parent, want) {
			t.Fatalf("got %q %v, want %q", got, err, filepath.Join(parent, want))
		}
		if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
			t.Fatalf("%s was not made: %v", got, err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "taken"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := newDir(parent, "taken"); err != nil || got != filepath.Join(parent, "taken-2") {
		t.Errorf("a file in the way: %q %v", got, err)
	}
	if got, err := newDir(filepath.Join(parent, "missing"), "gw"); err == nil {
		t.Errorf("a parent that isn't there gave %q", got)
	}
}

// A gateway's ports go back to the block when it ends, and are taken again only once the block
// has no port left that was never used: a single run gets fresh ports as before, and a test run
// three times in one process (-count=3) no longer runs out.
func TestPortBlockTakesBackWhatGatewaysGiveBack(t *testing.T) {
	always := func(int) bool { return true }
	b := &portBlock{low: 29100, high: 29113, taken: map[int]bool{}, free: always}
	first, err := b.take(7)
	if err != nil {
		t.Fatal(err)
	}
	b.release(first)
	second, err := b.take(7)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range second {
		if slicesContain(first, p) {
			t.Fatalf("port %d was taken again while fresh ones were left: %v then %v", p, first, second)
		}
	}
	b.release(second)
	third, err := b.take(7)
	if err != nil {
		t.Fatalf("a block whose ports were all given back: %v", err)
	}
	for i, p := range third {
		if p != first[i] {
			t.Errorf("given back first, taken back first: %v, then %v", first, third)
			break
		}
	}
	// A restart on the same ports takes them back, so they aren't handed out twice.
	small := &portBlock{low: 29100, high: 29106, taken: map[int]bool{}, free: always}
	ports, err := small.take(7)
	if err != nil {
		t.Fatal(err)
	}
	small.release(ports)
	small.reclaim(ports)
	if got, err := small.take(1); err == nil {
		t.Errorf("a port a restart holds was handed out again: %v", got)
	}
	// A port given back that something still listens on is passed over, and a take that can't be
	// met takes nothing.
	small.release(ports)
	small.free = func(p int) bool { return p != ports[0] }
	if got, err := small.take(7); err == nil {
		t.Errorf("a port still in use was handed out: %v", got)
	}
	if got, err := small.take(6); err != nil || slicesContain(got, ports[0]) {
		t.Errorf("the six free ports: %v %v", got, err)
	}
}

// Test binaries that go test runs side by side (-p) each have a block over the same ports: a port
// one has taken is one the other passes over.
func TestPortBlocksInTwoProcessesNeverShareAPort(t *testing.T) {
	locks := t.TempDir()
	always := func(int) bool { return true }
	a := &portBlock{low: 29100, high: 29104, taken: map[int]bool{}, free: always, lockDir: locks}
	b := &portBlock{low: 29100, high: 29104, taken: map[int]bool{}, free: always, lockDir: locks}
	mine, err := a.take(3)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := b.take(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range theirs {
		if slicesContain(mine, p) {
			t.Fatalf("both blocks took port %d: %v and %v", p, mine, theirs)
		}
	}
	if got, err := b.take(1); err == nil {
		t.Errorf("a port the other block holds was taken: %v", got)
	}
}

func slicesContain(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
