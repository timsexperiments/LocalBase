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
