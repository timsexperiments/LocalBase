package root

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
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
		if got := mode(t, dir); got != 0o700 {
			t.Errorf("after init on a %o directory: %o", loose, got)
		}
		if err := os.Chmod(dir, loose); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err != nil {
			t.Fatal(err)
		}
		if got := mode(t, dir); got != 0o700 {
			t.Errorf("after open on a %o root: %o", loose, got)
		}
	}
}

func TestNewRootIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "root")
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o700 {
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
	if got := mode(t, dir); got != 0o755 {
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
	if got := mode(t, dir); got != 0o755 {
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
			if _, err := Open(dir); err != nil {
				t.Errorf("group-writable is tightened at serve, not refused: %v", err)
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
	want := "Refusing to use " + file + ": other users can change it. If you trust it, run: chmod o-w '" + file + "'. If you did not put it there, remove it."
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
