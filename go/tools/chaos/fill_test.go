package chaos

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// What a full volume can do, planted: no volume here, and no waiting.
func TestFreeFullDisk(t *testing.T) {
	const size = 96 << 20
	full := syscall.ENOSPC
	times := func(n int, err error) func() error {
		return func() error {
			if n > 0 {
				n--
				return err
			}
			return nil
		}
	}
	for _, c := range []struct {
		name             string
		truncate, remove func() error
		freed            int64
		cuts             int
		want             string
	}{
		{"the delete just works, and nothing is cut", times(0, nil), times(0, nil), 1 << 20, 0, ""},
		{"the delete is refused for a while", times(0, nil), times(3, full), 1 << 20, 3, ""},
		{"the delete and the cut are both refused for a while", times(2, full), times(3, full), 1 << 20, 3, ""},
		{"the cut is always refused, and the delete comes through", times(99, full), times(4, full), 1 << 20, 4, ""},
		{"the delete is always refused", times(0, nil), times(99, full), 1 << 20, 10, "deleting the filler: still refused"},
		{"the room never comes back", times(0, nil), times(0, nil), 4096, 0, "under the floor"},
		{"a different failure of the delete is not waited out", times(0, nil), times(1, os.ErrPermission), 1 << 20, 0, "permission denied"},
		{"a different failure of the cut is not waited out", times(1, os.ErrPermission), times(1, full), 1 << 20, 1, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cuts := 0
			err := freeFullDisk("filler", size, diskOps{
				truncate: func(_ string, to int64) error {
					cuts++
					if to != size-givenBack {
						t.Errorf("cut to %d", to)
					}
					return c.truncate()
				},
				remove:    func(string) error { return c.remove() },
				available: func() (int64, error) { return c.freed, nil },
				wait:      func() {},
			}, 10)
			if cuts != c.cuts {
				t.Errorf("%d cuts, want %d", cuts, c.cuts)
			}
			if c.want == "" {
				if err != nil {
					t.Errorf("err %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want it to say %q", err, c.want)
			}
		})
	}
}

// The fill keeps a margin. It can't show here that the filesystem needs one: this Mac's APFS stops
// writes with 2.43 MiB still free, so a fill to the last bytes leaves room to delete anyway, and the
// refused delete has only been seen on macos-15 (#671's first run). It shows the code keeps one, at
// least the 4 MiB measured as real free space, with a probe larger than it.
func TestFillLeavesAMargin(t *testing.T) {
	if fillMargin < 4<<20 {
		t.Errorf("fillMargin is %d bytes, under the 4 MiB that is real free space on this Mac's APFS", fillMargin)
	}
	if fillProbe <= fillMargin {
		t.Errorf("the probe (%d bytes) must be larger than the margin (%d)", fillProbe, fillMargin)
	}
}

// A failure in CI lists each refused delete with the free bytes at that moment, runs put together.
func TestRefusalsReadAsRuns(t *testing.T) {
	got := runs([]string{"0 bytes free: no space", "0 bytes free: no space", "4096 bytes free: no space"})
	if want := "tries 1-2: 0 bytes free: no space; try 3: 4096 bytes free: no space"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	err := freeFullDisk("filler", 96<<20, diskOps{
		truncate:  func(string, int64) error { return nil },
		remove:    func(string) error { return syscall.ENOSPC },
		available: func() (int64, error) { return 0, nil },
		wait:      func() {},
	}, 3)
	if err == nil || !strings.Contains(err.Error(), "each try: tries 1-3: 0 bytes free: no space left on device") {
		t.Errorf("err %v, want each try with its free bytes", err)
	}
}

func TestDetachVolume(t *testing.T) {
	busy := []byte(`hdiutil: couldn't eject "disk4" - Resource busy`)
	script := func(fails int, out []byte) (func() ([]byte, error), *int) {
		calls := 0
		return func() ([]byte, error) {
			calls++
			if calls <= fails {
				return out, errors.New("exit status 16")
			}
			return nil, nil
		}, &calls
	}
	detach, calls := script(0, nil)
	if err := detachVolume(detach, func() {}, 5); err != nil || *calls != 1 {
		t.Errorf("at once: %v after %d", err, *calls)
	}
	detach, calls = script(3, busy)
	if err := detachVolume(detach, func() {}, 5); err != nil || *calls != 4 {
		t.Errorf("busy for a while: %v after %d", err, *calls)
	}
	detach, calls = script(99, busy)
	if err := detachVolume(detach, func() {}, 5); err == nil || !strings.Contains(err.Error(), "still busy after 5 tries") || *calls != 5 {
		t.Errorf("always busy: %v after %d", err, *calls)
	}
	detach, calls = script(99, []byte("hdiutil: detach failed - no such file"))
	if err := detachVolume(detach, func() {}, 5); err == nil || !strings.Contains(err.Error(), "no such file") || *calls != 1 {
		t.Errorf("any other failure is not waited out: %v after %d", err, *calls)
	}
}

// How the fill meets a volume that refuses, or reads low until a sync, planted: no volume, no
// waiting. macos-15 in CI ended a fill with 13.5 to 13.9 MiB free, and then took 5 MiB. The first
// two cases are the two ways that could happen; on main's fill, both end with that much free.
func TestFillToWaitsOutARefusal(t *testing.T) {
	const MiB = 1 << 20
	type volume struct {
		free, reserve int64 // writes are refused whole past free-reserve, as APFS refuses them
		refuseAt      int64 // one refusal, once free falls to this, that a sync clears
		hidden        int64 // free space that reads as used until the first sync
		syncs         int
		fail          error
	}
	ops := func(v *volume) fillOps {
		return fillOps{
			write: func(n int) (int, error) {
				switch {
				case v.fail != nil:
					return 0, v.fail
				case v.refuseAt > 0 && v.free <= v.refuseAt:
					v.refuseAt = 0
					return 0, syscall.ENOSPC
				case int64(n) > v.free-v.reserve:
					return 0, syscall.ENOSPC
				}
				v.free -= int64(n)
				return n, nil
			},
			sync: func() error { v.syncs++; v.hidden = 0; return nil },
			free: func() (int64, error) { return v.free - v.hidden, nil },
		}
	}
	const margin = 4 * MiB
	full := [2]int64{margin, margin + fillSmallest - 1}
	for _, c := range []struct {
		name    string
		v       volume
		limit   int
		ends    [2]int64 // where free must end
		notes   int
		wantErr string
	}{
		{"a refusal a sync clears, with 13.5 MiB free", volume{free: 90 * MiB, refuseAt: 27 * MiB / 2}, fillDiskLimit, full, 1, ""},
		{"10 MiB that reads as used until a sync", volume{free: 90 * MiB, hidden: 10 * MiB}, fillDiskLimit, full, 1, ""},
		{"no refusal: to the margin", volume{free: 90 * MiB}, fillDiskLimit, full, 0, ""},
		{"a volume that keeps 2.4 MiB back, under the margin", volume{free: 90 * MiB, reserve: 12 * MiB / 5}, fillDiskLimit, full, 0, ""},
		{"a volume that keeps 13.5 MiB back, over the margin", volume{free: 90 * MiB, reserve: 27 * MiB / 2}, fillDiskLimit, [2]int64{27 * MiB / 2, 27*MiB/2 + fillSmallest - 1}, 8 + fillStubborn, ""},
		{"a write that fails otherwise", volume{free: 90 * MiB, fail: syscall.EIO}, fillDiskLimit, [2]int64{90 * MiB, 90 * MiB}, 0, "input/output error"},
		{"a volume bigger than the limit", volume{free: 90 * MiB}, 10 * MiB, [2]int64{0, 90 * MiB}, 0, "still not full"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := c.v
			_, notes, err := fillTo(margin, c.limit, ops(&v))
			if (err == nil) != (c.wantErr == "") || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err %v, want %q", err, c.wantErr)
			}
			if v.free < c.ends[0] || v.free > c.ends[1] {
				t.Errorf("ended with %d free, want %d to %d; the fill met: %s", v.free, c.ends[0], c.ends[1], fillNotes(notes))
			}
			if len(notes) != c.notes {
				t.Errorf("%d notes, want %d: %s", len(notes), c.notes, fillNotes(notes))
			}
			if c.wantErr == "" && v.syncs == 0 {
				t.Errorf("the fill ended without a sync")
			}
		})
	}
}
