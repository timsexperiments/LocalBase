package root

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A root is private to the account that runs LocalBase, and what is in it decides who
// can sign in and what gets executed. So before anything in a root is used, the
// directory is closed to everyone else, and a root that someone else could already
// have written to is refused: tightening a mode afterwards does not undo a file
// planted while it was open.

// UnsafeRootError names what makes a root unsafe to use, and what to run about it.
type UnsafeRootError struct {
	Path    string
	Problem string
	Fix     string
}

func (e *UnsafeRootError) Error() string {
	// With no file to name, what's wrong is the whole sentence.
	if e.Path == "" {
		return e.Problem
	}
	return fmt.Sprintf("Refusing to use %s: %s. %s", e.Path, e.Problem, e.Fix)
}

const (
	otherUserOwns  = "another user owns it"
	othersCanWrite = "other users can change it"
	runningAsRoot  = "this command is running as root, and it belongs to another user"
)

// shellQuote makes a path safe to paste into a shell.
// ShellQuote quotes s to paste into a shell.
func ShellQuote(s string) string { return shellQuote(s) }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runAs is how to name a user to sudo.
func runAs(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username
	}
	return "'#" + strconv.Itoa(uid) + "'"
}

// ownedByOther explains a path another user owns. Running as root is usually sudo out
// of habit, and would leave files the owner cannot read, so that gets its own answer.
func ownedByOther(path string, owner int, isDir bool, o options) *UnsafeRootError {
	if o.currentUID() == 0 {
		who := runAs(owner)
		return &UnsafeRootError{path, runningAsRoot, "Run local-base without sudo, as the user who owns it: sudo -u " + who + " local-base ..."}
	}
	chown := "sudo chown "
	if isDir {
		chown += "-R "
	}
	return &UnsafeRootError{path, otherUserOwns, "If it is yours, run: " + chown + `"$(id -un)" ` + shellQuote(path) + ". If you did not put it there, remove it."}
}

// writableByOthers explains a path other users can change.
func writableByOthers(path string) *UnsafeRootError {
	return &UnsafeRootError{path, othersCanWrite, "If you trust it, run: chmod go-w " + shellQuote(path) + ". If you did not put it there, remove it."}
}

// WritableByOthers reports whether anyone but the owner could change a file or
// directory, going by its mode. Others always count. A group counts unless it is provably
// the owner's alone, because a group usually means other people: on macOS a file's group
// is normally staff, the primary group of every local account. The case that is harmless
// is a Linux user-private group under umask 002, where a hand-edited file comes out 0664
// and nobody else is in the group. Pass an Lstat result: a symbolic link's own mode means
// nothing, and opening some files tightens them first.
func WritableByOthers(info fs.FileInfo) bool { return writableByOthersWith(info, options{}) }

func writableByOthersWith(info fs.FileInfo, o options) bool {
	perm := info.Mode().Perm()
	switch {
	case perm&0o002 != 0:
		return true
	case perm&0o020 == 0:
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || !groupIsPrivate(int(st.Gid), o)
}

// groupIsPrivate reports whether a group is provably this user's alone: it is their
// primary group, it has their name, nobody else is listed in it or has it as a primary
// group, and accounts come only from local files. The standard library can't list a
// group's members, and accounts from a directory service don't appear in these files, so
// anything that can't be established this way is a no. That includes macOS, whose
// accounts aren't in these files.
func groupIsPrivate(gid int, o options) bool {
	if !accountsAreLocal(o) {
		return false
	}
	want := strconv.Itoa(gid)
	me, uid := "", strconv.Itoa(o.currentUID())
	ok := readAccounts(o.etc(o.passwd, "/etc/passwd"), func(f []string) bool { // name:password:uid:gid:...
		if len(f) < 4 {
			return false
		}
		switch {
		case f[2] == uid:
			me = f[0]
			return f[3] == want
		case f[3] == want:
			return false // someone else's primary group
		}
		return true
	})
	if !ok || me == "" {
		return false
	}
	found := false
	ok = readAccounts(o.etc(o.group, "/etc/group"), func(f []string) bool { // name:password:gid:members
		if len(f) < 4 {
			return false
		}
		if f[2] != want {
			return true
		}
		found = true
		if f[0] != me {
			return false
		}
		for _, member := range strings.Split(f[3], ",") {
			if member != "" && member != me {
				return false
			}
		}
		return true
	})
	return ok && found
}

// readAccounts calls fn on each entry of an account file until it answers false, and
// reports whether the file was read to its end. An entry starting with + or - pulls
// accounts in from elsewhere, so nothing can be concluded from such a file.
func readAccounts(path string, fn func(fields []string) bool) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#':
			continue
		case line[0] == '+' || line[0] == '-':
			return false
		}
		if !fn(strings.Split(line, ":")) {
			return false
		}
	}
	return true
}

// accountsAreLocal reports whether users and groups come only from local files. Without
// a configuration file they do (musl has no name service switch).
func accountsAreLocal(o options) bool {
	b, err := os.ReadFile(o.etc(o.nsswitch, "/etc/nsswitch.conf"))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		key, sources, ok := strings.Cut(line, ":")
		if key = strings.TrimSpace(key); !ok || (key != "passwd" && key != "group") {
			continue
		}
		for _, source := range strings.Fields(sources) {
			switch {
			case strings.HasPrefix(source, "["):
			case source == "files", source == "compat", source == "systemd", source == "altfiles":
			default:
				return false
			}
		}
	}
	return true
}

// The files a root trusts, relative to it. Files LocalBase writes are 0600, so one
// that others can write did not come from here. The database is not checked for that:
// older versions created it under the umask, and it is tightened where it opens.
var (
	trustedFiles = []string{markerName, RestoreJournalName, "ui-access.json", "email-delivery.json", "catalog-feed.json",
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

func checkEntry(path string, writableIsFine bool, o options) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	uid, ok := ownerOf(info)
	if !ok {
		uid = -1
	}
	if uid = o.ownerOf(path, uid); uid != o.currentUID() {
		return ownedByOther(path, uid, info.IsDir(), o)
	}
	// A symbolic link's own mode means nothing; its owner is what was checked.
	if !writableIsFine && info.Mode()&fs.ModeSymlink == 0 && writableByOthersWith(info, o) {
		return writableByOthers(path)
	}
	return nil
}

// checkOwner refuses a directory this user does not own. It changes nothing, so it can
// run before anything says this is a root at all.
func checkOwner(dir string, info fs.FileInfo, o options) error {
	uid, ok := ownerOf(info)
	if !ok {
		uid = -1
	}
	if uid = o.ownerOf(dir, uid); uid != o.currentUID() {
		return ownedByOther(dir, uid, true, o)
	}
	return nil
}

// closeDirectory refuses a directory this user does not own, and closes it to others.
// What decides is the mode it has afterwards, since some volumes accept a chmod and ignore
// it. A directory others can still change stops the root, unless the volume is read-only,
// where nobody can. One they can only read does not: the files in it have their own modes.
func closeDirectory(dir string, o options) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := checkOwner(dir, info, o); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := o.chmodDirectory(dir, 0o700); errors.Is(err, syscall.EROFS) {
		return nil
	}
	if info, err = os.Stat(dir); err != nil {
		return err
	}
	if writableByOthersWith(info, o) {
		return writableByOthers(dir)
	}
	return nil
}

// checkBeforeReading gives a root someone else owns its own answer, which an unreadable
// marker would otherwise hide behind "permission denied". A path that can't be examined
// is left for the read that follows to report.
func checkBeforeReading(dir string, o options) error {
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	if err := checkOwner(dir, info, o); err != nil {
		return err
	}
	return checkEntry(filepath.Join(dir, markerName), false, o)
}

// secure checks an existing root before anything in it is used.
func secure(dir string, o options) error {
	if err := closeDirectory(dir, o); err != nil {
		return err
	}
	for _, name := range trustedFiles {
		if err := checkEntry(filepath.Join(dir, name), false, o); err != nil {
			return err
		}
	}
	for _, name := range databaseFiles {
		if err := checkEntry(filepath.Join(dir, name), true, o); err != nil {
			return err
		}
	}
	for _, name := range trustedDirs {
		if err := checkEntry(filepath.Join(dir, name), false, o); err != nil {
			return err
		}
	}
	for _, name := range trustedListings {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			continue // a missing or unreadable listing is reported where it is read
		}
		for _, e := range entries {
			if err := checkEntry(filepath.Join(dir, name, e.Name()), false, o); err != nil {
				return err
			}
		}
	}
	return nil
}
