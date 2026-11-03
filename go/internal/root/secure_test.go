package root

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func secureMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// A directory that is already there is closed to others by init, and by any later
// use of the root: nothing waits for the gateway to start.
func TestRootIsClosedToOthers(t *testing.T) {
	for _, loose := range []os.FileMode{0o755, 0o775, 0o777} {
		dir := tempRoot(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, loose); err != nil {
			t.Fatal(err)
		}
		if _, err := Init(dir); err != nil {
			t.Fatal(err)
		}
		if got := secureMode(t, dir); got != 0o700 {
			t.Errorf("after init on a %o directory: %o", loose, got)
		}
		if err := os.Chmod(dir, loose); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err != nil {
			t.Fatal(err)
		}
		if got := secureMode(t, dir); got != 0o700 {
			t.Errorf("after open on a %o root: %o", loose, got)
		}
	}
}

func TestNewRootIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "root")
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if got := secureMode(t, dir); got != 0o700 {
		t.Errorf("%o", got)
	}
}

// Opening a directory that is not a root changes nothing about it.
func TestOpenLeavesOtherDirectoriesAlone(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	if _, err := Open(dir); err == nil {
		t.Fatal("opened a directory with no marker")
	}
	if got := secureMode(t, dir); got != 0o755 {
		t.Errorf("%o", got)
	}
}

func TestRootOwnedByAnotherUserIsRefused(t *testing.T) {
	dir := tempRoot(t)
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	me := currentUID()
	currentUID = func() int { return me + 1 }
	defer func() { currentUID = os.Getuid }()
	var unsafe *UnsafeRootError
	if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Problem != otherUserOwns {
		t.Errorf("open: %v", err)
	}
	if _, err := Init(dir); !errors.As(err, &unsafe) {
		t.Errorf("init on an existing root: %v", err)
	}
	if got := secureMode(t, dir); got != 0o755 {
		t.Errorf("changed the mode of a root it refused: %o", got)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(empty); !errors.As(err, &unsafe) {
		t.Errorf("init on another user's empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, markerName)); err == nil {
		t.Error("wrote a marker into another user's directory")
	}
}

// This repeats the check on a file another user really owns, where one exists.
func TestRealOwnerIsRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	if err := checkEntry("/", true); err == nil {
		t.Error("/ is not this user's")
	}
}

// Every file a root trusts is refused if someone else could have put it there.
func TestPlantedFilesAreRefused(t *testing.T) {
	entries := []string{"ui-access.json", "email-delivery.json", "catalog-feed.json", filepath.Join("credentials", "auth-profiles.json"),
		filepath.Join("runtime", "ui-csrf.key"), filepath.Join("service", "manifest.json"), filepath.Join("backends.d", "planted.json"), filepath.Join("catalog.d", "planted.json")}
	setup := func(t *testing.T, name string) (string, string) {
		t.Helper()
		dir := tempRoot(t)
		if _, err := Init(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err != nil {
			t.Fatalf("this user's own %s: %v", name, err)
		}
		return dir, path
	}
	for _, name := range entries {
		t.Run(name, func(t *testing.T) {
			dir, path := setup(t, name)
			if err := os.Chmod(path, 0o666); err != nil {
				t.Fatal(err)
			}
			var unsafe *UnsafeRootError
			if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != path || unsafe.Problem != othersCanWrite {
				t.Errorf("writable by others: %v", err)
			}
			if err := os.Chmod(path, 0o660); err != nil {
				t.Fatal(err)
			}
			// Group-writable: refused, unless the group is provably the owner's alone.
			privateGroup(t, path)
			if _, err := Open(dir); err != nil {
				t.Errorf("group-writable in a private group: %v", err)
			}
			accounts(t, "", "", "")
			if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != path || unsafe.Problem != othersCanWrite {
				t.Errorf("group-writable in a group that may not be private: %v", err)
			}
		})
	}
	// The same, for a file another user owns.
	t.Run("owner", func(t *testing.T) {
		dir, path := setup(t, entries[0])
		const me = 1000
		currentUID = func() int { return me }
		statOwnerHook = func(p string, _ int) int {
			if p == path {
				return me + 1
			}
			return me
		}
		defer func() {
			currentUID = os.Getuid
			statOwnerHook = func(_ string, uid int) int { return uid }
		}()
		var unsafe *UnsafeRootError
		if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != path || unsafe.Problem != otherUserOwns {
			t.Errorf("%v", err)
		}
	})
}

func TestTrustedDirectoriesAreChecked(t *testing.T) {
	for _, name := range []string{"credentials", "runtime", "service", "bin", "backends.d", "catalog.d"} {
		dir := tempRoot(t)
		if _, err := Init(dir); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
		var unsafe *UnsafeRootError
		if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != path {
			t.Errorf("%s writable by others: %v", name, err)
		}
	}
}

// The database and a symbolic link are judged by owner alone: older versions left the
// database under the umask, and a link's own mode means nothing.
func TestDatabaseAndLinksAreJudgedByOwner(t *testing.T) {
	dir := tempRoot(t)
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "local-base.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0o666); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "ui-access.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Error(err)
	}
	statOwnerHook = func(p string, uid int) int {
		if p == db {
			return uid + 1
		}
		return uid
	}
	defer func() { statOwnerHook = func(_ string, uid int) int { return uid } }()
	if _, err := Open(dir); err == nil {
		t.Error("a database another user owns")
	}
}

// Only what a root trusts is checked: a model file may be shared.
func TestOtherFilesAreNotJudged(t *testing.T) {
	dir := tempRoot(t)
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "models", "llm"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "models", "llm", "m.gguf")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Error(err)
	}
}

// tempRoot is a path for a root, resolved as a root's own path is.
func tempRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "root")
}

// A refusal says which path and exactly what to run: it stops every command, so the
// person reading it needs to be able to act on it.
func TestRefusalsSayWhatToRun(t *testing.T) {
	dir := tempRoot(t)
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	// Owners are injected, so this reads the same whoever runs the test, root included.
	const me = 1000
	owned := func(other string) func(string, int) int {
		return func(p string, _ int) int {
			if p == other {
				return me + 1
			}
			return me
		}
	}
	currentUID = func() int { return me }
	statOwnerHook = owned("")
	defer func() {
		currentUID = os.Getuid
		statOwnerHook = func(_ string, uid int) int { return uid }
	}()
	file := filepath.Join(dir, "ui-access.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Others can change a file.
	if err := os.Chmod(file, 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	want := "Refusing to use " + file + ": other users can change it. If you trust it, run: chmod go-w '" + file + "'. If you did not put it there, remove it."
	if err == nil || err.Error() != want {
		t.Errorf("writable file:\n got %v\nwant %s", err, want)
	}
	os.Chmod(file, 0o600)

	// Another user owns a file.
	statOwnerHook = owned(file)
	_, err = Open(dir)
	want = "Refusing to use " + file + ": another user owns it. If it is yours, run: sudo chown \"$(id -un)\" '" + file + "'. If you did not put it there, remove it."
	if err == nil || err.Error() != want {
		t.Errorf("another user's file:\n got %v\nwant %s", err, want)
	}

	// Another user owns the whole root: every file in it needs correcting.
	statOwnerHook = owned(dir)
	_, err = Open(dir)
	want = "Refusing to use " + dir + ": another user owns it. If it is yours, run: sudo chown -R \"$(id -un)\" '" + dir + "'. If you did not put it there, remove it."
	if err == nil || err.Error() != want {
		t.Errorf("another user's root:\n got %v\nwant %s", err, want)
	}

	// Run with sudo: chown would be the wrong advice.
	statOwnerHook = owned("")
	currentUID = func() int { return 0 }
	_, err = Open(dir)
	var unsafe *UnsafeRootError
	if !errors.As(err, &unsafe) || unsafe.Problem != runningAsRoot ||
		!strings.Contains(err.Error(), "Run local-base without sudo") || !strings.Contains(err.Error(), "sudo -u ") || strings.Contains(err.Error(), "chown") {
		t.Errorf("as root: %v", err)
	}

	// A path a shell would misread is quoted.
	if got := shellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Errorf("%s", got)
	}
	// A user who cannot be named is still named to sudo.
	if got := runAs(-7); !strings.Contains(got, "#-7") {
		t.Errorf("%s", got)
	}
}

// accounts points the account files at these contents, for this test. An empty string
// leaves that file out.
func accounts(t *testing.T, passwd, group, nsswitch string) {
	t.Helper()
	dir := t.TempDir()
	set := func(target *string, name, content string) {
		old := *target
		*target = filepath.Join(dir, name)
		if content != "" {
			if err := os.WriteFile(*target, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { *target = old })
	}
	set(&etcPasswd, "passwd", passwd)
	set(&etcGroup, "group", group)
	set(&etcNSSwitch, "nsswitch", nsswitch)
}

func groupOf(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Sys().(*syscall.Stat_t).Gid)
}

// privateGroup makes a path's group look like this user's own private group.
func privateGroup(t *testing.T, path string) {
	t.Helper()
	gid := groupOf(t, path)
	accounts(t, fmt.Sprintf("me:x:%d:%d::/home/me:/bin/sh\n", os.Getuid(), gid), fmt.Sprintf("me:x:%d:\n", gid), "")
}

// A group counts as other people unless it is provably the owner's alone.
func TestGroupWritableDependsOnWhoIsInTheGroup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o664); err != nil {
		t.Fatal(err)
	}
	gid, uid := groupOf(t, file), os.Getuid()
	me := fmt.Sprintf("me:x:%d:%d::/home/me:/bin/sh\n", uid, gid)
	mine := fmt.Sprintf("me:x:%d:\n", gid)
	for _, tc := range []struct {
		name                    string
		passwd, group, nsswitch string
		othersCanWrite          bool
	}{
		{"the owner's own group", me, mine, "", false},
		{"listing only the owner", me, fmt.Sprintf("me:x:%d:me\n", gid), "", false},
		{"among other accounts", fmt.Sprintf("daemon:x:%d:%d::/:/bin/sh\n# a comment\n\n", uid+100, gid+100) + me, fmt.Sprintf("daemon:x:%d:\n", gid+100) + mine, "", false},
		{"another member", me, fmt.Sprintf("me:x:%d:me,other\n", gid), "", true},
		{"only another member", me, fmt.Sprintf("me:x:%d:other\n", gid), "", true},
		{"another name", me, fmt.Sprintf("staff:x:%d:\n", gid), "", true},
		{"someone else's primary group", me + fmt.Sprintf("other:x:%d:%d::/home/o:/bin/sh\n", uid+1, gid), mine, "", true},
		{"not the owner's primary group", fmt.Sprintf("me:x:%d:%d::/home/me:/bin/sh\n", uid, gid+1), mine, "", true},
		{"the group is not listed", me, fmt.Sprintf("me:x:%d:\n", gid+1), "", true},
		{"the same id under a second name", me, mine + fmt.Sprintf("shared:x:%d:\n", gid), "", true},
		{"the owner is not listed", fmt.Sprintf("daemon:x:%d:%d::/:/bin/sh\n", uid+100, gid+100), mine, "", true},
		{"the owner is not listed, and the group has no name", fmt.Sprintf("daemon:x:%d:%d::/:/bin/sh\n", uid+100, gid+100), fmt.Sprintf(":x:%d:\n", gid), "", true},
		{"no user file", "", mine, "", true},
		{"no group file", me, "", "", true},
		{"a malformed user entry", me + "broken\n", mine, "", true},
		{"a malformed group entry", me, mine + "broken\n", "", true},
		{"users pulled in from elsewhere", me + "+::::::\n", mine, "", true},
		{"groups pulled in from elsewhere", me, mine + "+:::\n", "", true},
		{"local sources", me, mine, "passwd: files systemd\ngroup: compat altfiles # local\nhosts: dns\n", false},
		{"actions between sources", me, mine, "group: files [SUCCESS=merge] systemd\n", false},
		{"groups from a directory", me, mine, "group: files ldap\n", true},
		{"users from a directory", me, mine, "passwd: sss files\n", true},
		{"other maps may use anything", me, mine, "hosts: files dns\n# group: ldap\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts(t, tc.passwd, tc.group, tc.nsswitch)
			info, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			if got := WritableByOthers(info); got != tc.othersCanWrite {
				t.Errorf("WritableByOthers = %v, want %v", got, tc.othersCanWrite)
			}
		})
	}
	t.Run("an unreadable name service configuration", func(t *testing.T) {
		accounts(t, me, mine, "")
		dir := t.TempDir() // reading a directory fails with something other than not-found
		old := etcNSSwitch
		etcNSSwitch = dir
		t.Cleanup(func() { etcNSSwitch = old })
		info, _ := os.Lstat(file)
		if !WritableByOthers(info) {
			t.Error("unknown must mean refuse")
		}
	})
	// Others can write: no group makes that fine. Nobody else can: no group matters.
	accounts(t, me, mine, "")
	for perm, want := range map[os.FileMode]bool{0o666: true, 0o646: true, 0o602: true, 0o664: false, 0o644: false, 0o600: false} {
		if err := os.Chmod(file, perm); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Lstat(file)
		if got := WritableByOthers(info); got != want {
			t.Errorf("%o in a private group: %v", perm, got)
		}
	}
	accounts(t, "", "", "")
	for perm, want := range map[os.FileMode]bool{0o664: true, 0o660: true, 0o644: false, 0o600: false, 0o640: false} {
		if err := os.Chmod(file, perm); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Lstat(file)
		if got := WritableByOthers(info); got != want {
			t.Errorf("%o in an unknown group: %v", perm, got)
		}
	}
}

// On macOS, whose accounts aren't in these files and whose files are normally in the
// staff group shared by every local account, group-writable always counts.
func TestMacOSNeverCountsAGroupAsPrivate(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o664); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(file)
	if !WritableByOthers(info) {
		t.Error("a group-writable file on macOS")
	}
}

// A refusal for a group says what to run, as one for others does.
func TestGroupWritableRefusalSaysWhatToRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	path := filepath.Join(dir, "backends.d")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "mine.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o664); err != nil {
		t.Fatal(err)
	}
	accounts(t, "", "", "")
	want := "Refusing to use " + file + ": other users can change it. If you trust it, run: chmod go-w '" + file + "'. If you did not put it there, remove it."
	if _, err := Open(dir); err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
	// Following that advice is enough.
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("after the fix: %v", err)
	}
}

// A root someone else owns gets its own answer, even when its marker can't be read.
func TestOpenChecksOwnersBeforeReadingTheMarker(t *testing.T) {
	setup := func(t *testing.T) (dir, marker string) {
		t.Helper()
		dir = filepath.Join(t.TempDir(), "root")
		if _, err := Init(dir); err != nil {
			t.Fatal(err)
		}
		dir, _ = filepath.EvalSymlinks(dir)
		marker = filepath.Join(dir, markerName)
		if err := os.Chmod(marker, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(marker, 0o600) })
		return dir, marker
	}
	owner := func(t *testing.T, path string) {
		old := statOwnerHook
		statOwnerHook = func(p string, uid int) int {
			if p == path {
				return uid + 1
			}
			return uid
		}
		t.Cleanup(func() { statOwnerHook = old })
	}
	t.Run("the root", func(t *testing.T) {
		dir, _ := setup(t)
		owner(t, dir)
		var unsafe *UnsafeRootError
		if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != dir || (unsafe.Problem != otherUserOwns && unsafe.Problem != runningAsRoot) {
			t.Errorf("%v", err)
		}
	})
	t.Run("the marker", func(t *testing.T) {
		dir, marker := setup(t)
		owner(t, marker)
		var unsafe *UnsafeRootError
		if _, err := Open(dir); !errors.As(err, &unsafe) || unsafe.Path != marker || (unsafe.Problem != otherUserOwns && unsafe.Problem != runningAsRoot) {
			t.Errorf("%v", err)
		}
	})
}

// Asking whether a directory is a root doesn't change it.
func TestOpenLeavesADirectoryThatIsNotARootAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "not an initialized LocalBase root") {
		t.Errorf("%v", err)
	}
	if got := secureMode(t, dir); got != 0o755 {
		t.Errorf("a directory that isn't a root is now %o", got)
	}
}

// A directory that stays open to others stops the root, however the chmod went.
func TestADirectoryThatCannotBeClosed(t *testing.T) {
	failing := func(err error) func(string, os.FileMode) error {
		return func(string, os.FileMode) error { return err }
	}
	cases := []struct {
		name    string
		mode    os.FileMode
		chmod   func(string, os.FileMode) error
		refused bool
	}{
		{"chmod refused, others can write", 0o777, failing(syscall.EPERM), true},
		{"chmod accepted and ignored, others can write", 0o777, failing(nil), true},
		{"chmod refused, a shared group can write", 0o770, failing(syscall.EPERM), true},
		{"chmod refused, others can only read", 0o755, failing(syscall.EPERM), false},
		{"chmod accepted and ignored, others can only read", 0o755, failing(nil), false},
		{"a read-only volume", 0o777, failing(syscall.EROFS), false},
	}
	realGroup := etcGroup
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := tempRoot(t)
			if _, err := Init(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, c.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { etcGroup, chmodDirectory = realGroup, os.Chmod; os.Chmod(dir, 0o700) })
			etcGroup = filepath.Join(t.TempDir(), "no-such-file") // nothing can show the group is private
			chmodDirectory = c.chmod
			_, err := Open(dir)
			var unsafe *UnsafeRootError
			switch {
			case c.refused && (!errors.As(err, &unsafe) || unsafe.Problem != othersCanWrite || !strings.Contains(unsafe.Fix, "chmod go-w")):
				t.Errorf("expected a refusal naming the fix, got %v", err)
			case !c.refused && err != nil:
				t.Errorf("expected no refusal, got %v", err)
			}
		})
	}
}
