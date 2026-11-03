package root

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// A root is private to the account that runs LocalBase, and what is in it decides who
// can sign in and what gets executed. So before anything in a root is used, the
// directory is closed to everyone else, and a root that someone else could already
// have written to is refused: tightening a mode afterwards does not undo a file
// planted while it was open.

// UnsafeRootError names what makes a root unsafe to use.
type UnsafeRootError struct {
	Path    string
	Problem string
}

func (e *UnsafeRootError) Error() string {
	return fmt.Sprintf("Refusing to use %s: %s. Remove it or correct it, and check who can write to %s.", e.Path, e.Problem, filepath.Dir(e.Path))
}

const (
	otherUserOwns  = "another user owns it"
	othersCanWrite = "other users can change it"
)

var (
	currentUID = os.Getuid
	// statOwnerHook lets a test give one file another owner.
	statOwnerHook = func(_ string, uid int) int { return uid }
)

// The files a root trusts, relative to it. Files LocalBase writes are 0600, so one
// that others can write did not come from here. The database is not checked for that:
// older versions created it under the umask, and it is tightened where it opens.
var (
	trustedFiles = []string{markerName, "ui-access.json", "email-delivery.json", "catalog-feed.json",
		filepath.Join("credentials", "auth-profiles.json"), filepath.Join("runtime", "ui-csrf.key"), filepath.Join("service", "manifest.json")}
	databaseFiles = []string{"local-base.db", "local-base.db-wal", "local-base.db-shm"}
	trustedDirs   = []string{"credentials", "runtime", "service", "bin", "backends.d", "catalog.d"}
	// Every entry of these is read as configuration, and a backend names a program.
	trustedListings = []string{"backends.d", "catalog.d"}
)

func ownerOf(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func checkEntry(path string, writableIsFine bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if uid, ok := ownerOf(info); !ok || statOwnerHook(path, uid) != currentUID() {
		return &UnsafeRootError{path, otherUserOwns}
	}
	// A symbolic link's own mode means nothing; its owner is what was checked.
	if !writableIsFine && info.Mode()&fs.ModeSymlink == 0 && info.Mode().Perm()&0o002 != 0 {
		return &UnsafeRootError{path, othersCanWrite}
	}
	return nil
}

// closeDirectory refuses a directory this user does not own, and closes it to others.
func closeDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if uid, ok := ownerOf(info); !ok || uid != currentUID() {
		return &UnsafeRootError{dir, otherUserOwns}
	}
	if info.Mode().Perm()&0o077 != 0 {
		// A failure is not fatal: a read-only volume cannot be changed by anyone
		// here either, and serve reports what it could not tighten.
		os.Chmod(dir, 0o700)
	}
	return nil
}

// secure checks an existing root before anything in it is used.
func secure(dir string) error {
	if err := closeDirectory(dir); err != nil {
		return err
	}
	for _, name := range trustedFiles {
		if err := checkEntry(filepath.Join(dir, name), false); err != nil {
			return err
		}
	}
	for _, name := range databaseFiles {
		if err := checkEntry(filepath.Join(dir, name), true); err != nil {
			return err
		}
	}
	for _, name := range trustedDirs {
		if err := checkEntry(filepath.Join(dir, name), false); err != nil {
			return err
		}
	}
	for _, name := range trustedListings {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			continue // a missing or unreadable listing is reported where it is read
		}
		for _, e := range entries {
			if err := checkEntry(filepath.Join(dir, name, e.Name()), false); err != nil {
				return err
			}
		}
	}
	return nil
}
