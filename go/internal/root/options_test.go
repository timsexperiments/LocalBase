package root

import (
	"os"
	"path/filepath"
	"testing"
)

// options' zero value is how the package runs: this user's id, the owner a file has, os.Chmod
// and the system's account files. A set hook is used.
func TestTheOptionsZeroValueIsProduction(t *testing.T) {
	var o options
	if o.currentUID() != os.Getuid() || o.ownerOf("/x", 7) != 7 ||
		o.etc(o.passwd, "/etc/passwd") != "/etc/passwd" || o.etc(o.nsswitch, "/etc/nsswitch.conf") != "/etc/nsswitch.conf" {
		t.Error("options{} isn't production")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := o.chmodDirectory(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("options{} didn't chmod: %o", info.Mode().Perm())
	}
	set := options{uid: func() int { return 4 }, owner: func(string, int) int { return 5 }, passwd: "/p",
		chmod: func(string, os.FileMode) error { return os.ErrPermission }}
	if set.currentUID() != 4 || set.ownerOf("/x", 7) != 5 || set.etc(set.passwd, "/etc/passwd") != "/p" ||
		set.chmodDirectory(dir, 0o700) != os.ErrPermission {
		t.Error("set options aren't used")
	}
}
