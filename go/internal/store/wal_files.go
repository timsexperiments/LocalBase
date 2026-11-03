package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// ownerSuffix names the file beside the database saying which file its -wal belongs to.
const ownerSuffix = "-wal-owner"

// walHeaderMode is byte 18 of a database, its write version, when it is in write-ahead logging.
const walHeaderMode = 2

// fileID says which file this is, whatever it's called. A copy or a restored file is another one.
func fileID(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("no file identity here")
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10), nil
}

// recordWALOwner notes which file the -wal belongs to. It can't be done on a read-only root, which
// writes no -wal either. Failing here only makes checkWALBelongs unable to tell.
func recordWALOwner(path string) {
	id, err := fileID(path)
	if err != nil {
		return
	}
	owner := path + ownerSuffix
	if b, err := os.ReadFile(owner); err == nil && string(b) == id {
		return
	}
	tmp := owner + ".tmp"
	if os.WriteFile(tmp, []byte(id), 0o600) == nil && os.Rename(tmp, owner) != nil {
		_ = os.Remove(tmp) // it stays unable to tell, as above
	}
}

// checkWALBelongs refuses a -wal with commits in it that came with a different database file. A -wal
// says nothing of which database it belongs to: SQLite would replay it into this one and damage it
// (https://www.sqlite.org/howtocorrupt.html, "Mispairing database files and hot journals"). This
// catches a database moved or restored into place. It can't see one overwritten in place, one deleted
// and written again if the filesystem gives it the old inode number, as Linux can, or a -wal copied in
// with its owner file. A restore's own guarantee is that it leaves no -wal behind.
func checkWALBelongs(path string) error {
	wal := path + "-wal"
	info, err := os.Stat(wal)
	if err != nil || info.Size() == 0 {
		return nil
	}
	recorded, err := os.ReadFile(path + ownerSuffix)
	if err != nil {
		return nil // no record to compare, as on a root that hasn't opened with this build
	}
	id, err := fileID(path)
	if err != nil || id == string(recorded) {
		return nil
	}
	return fmt.Errorf("%s holds writes that may belong to a different copy of this database, so LocalBase didn't open it: replaying them could damage it. If it doesn't belong here, move it somewhere safe and try again. If it does, as after copying a whole root, remove %s and try again", wal, path+ownerSuffix)
}

// readOnlyRoot: this failed because nothing can be written here, not because the database is busy.
func readOnlyRoot(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqlite3.SQLITE_READONLY || code == sqlite3.SQLITE_CANTOPEN
}

// readablePath is the file to read for a read-only open. Opening a database in write-ahead logging
// with no process attached makes a -shm and a -wal, which stay, and fails where they can't be made. So
// then this reads a private copy of the database and its -wal. With a -shm there, a process is attached
// or left it, and the file can be read as it is. stage is what to remove afterwards.
func readablePath(path string) (stage, read string, err error) {
	if !inWAL(path) {
		return "", path, nil
	}
	if _, err := os.Stat(path + "-shm"); err == nil {
		return "", path, nil
	}
	// A copy of files someone is writing can be torn, so it's taken only with nobody holding the root.
	if root.InUse(filepath.Dir(path)) {
		return "", "", ErrInUse
	}
	parent := stageParent()
	sweepStages(parent, time.Now())
	stage, err = os.MkdirTemp(parent, stagePrefix+strconv.Itoa(os.Getpid())+"-") // 0700
	if err != nil {
		return "", "", err
	}
	read = filepath.Join(stage, filepath.Base(path))
	if err := copyPrivate(path, read); err != nil {
		removeStage(stage)
		return "", "", err
	}
	if err := copyPrivate(path+"-wal", read+"-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		removeStage(stage)
		return "", "", err
	}
	fmt.Fprintln(readNotice, "Read a copy of this root's database, as it stood just now.")
	return stage, read, nil
}

// readNotice is where a read of a copy says so. A variable so a test can read it.
var readNotice io.Writer = os.Stderr

const (
	stagePrefix = "localbase-read-"
	// stageMaxAge is how old a copy with no living owner is before anyone else removes it.
	stageMaxAge = time.Hour
)

// stageParent is where a copy goes: the user's cache folder, or the temp folder if there's none.
func stageParent() string {
	if cache, err := os.UserCacheDir(); err == nil {
		parent := filepath.Join(cache, "local-base")
		if os.MkdirAll(parent, 0o700) == nil {
			return parent
		}
	}
	return os.TempDir()
}

// sweepStages removes copies a crash left: they can hold secrets. Only ones past stageMaxAge whose
// process has gone, named as this code names them.
func sweepStages(parent string, now time.Time) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), stagePrefix)
		if !ok || !e.IsDir() {
			continue
		}
		pidText, _, _ := strings.Cut(name, "-")
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < stageMaxAge {
			continue
		}
		if syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, e.Name()))
	}
}

func inWAL(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() // only read
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return false
	}
	return strings.HasPrefix(string(header), "SQLite format 3") && header[18] == walHeaderMode
}

func copyPrivate(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // only read
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() // the copy has already failed
		return err
	}
	return out.Close()
}

// removeStage removes a private copy: it can hold secrets. A crash leaves it, private, for sweepStages.
func removeStage(stage string) {
	if stage != "" {
		_ = os.RemoveAll(stage)
	}
}
