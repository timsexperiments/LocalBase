package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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

// ownerSuffix names the file beside the database saying which database its -wal belongs to.
const ownerSuffix = "-wal-owner"

// walHeaderMode is byte 18 of a database, its write version, when it is in write-ahead logging.
const walHeaderMode = 2

// tokenOffset is where the database header holds application_id, which this uses as a token for the
// database: unlike an inode, it can't be reused by a new file, and it goes with the file when it's
// copied. It's read from the bytes of the main file, because SQLite would answer from the -wal being judged.
const tokenOffset = 68

// fileToken is the token in the main file's own header, 0 if it has none.
func fileToken(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // only read
	b := make([]byte, 4)
	if _, err := f.ReadAt(b, tokenOffset); errors.Is(err, io.EOF) {
		return 0, nil // a new, empty file
	} else if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// walIsEmpty: no commits are waiting in a -wal, so nobody else is writing and nothing needs judging.
func walIsEmpty(path string) bool {
	info, err := os.Stat(path + "-wal")
	return err != nil || info.Size() == 0
}

// claimWAL gives a database its token if it has none, and records whose -wal this is. It runs on a
// clean, writable open only, the -wal having been empty when it began: with commits in it, this can't
// know whose they are. Failing here only leaves checkWALBelongs unable to tell until the next open, so
// nothing here stops the open.
func (s *Store) claimWAL(ctx context.Context, path string) {
	token, err := fileToken(path)
	if err != nil {
		return
	}
	if token == 0 {
		if err := s.setToken(ctx); err != nil {
			return
		}
		if token, err = fileToken(path); err != nil || token == 0 {
			return // another process's commits kept the checkpoint from finishing: the next open
		}
	}
	owner := path + ownerSuffix
	id := strconv.FormatUint(uint64(token), 10)
	if b, err := os.ReadFile(owner); err == nil && string(b) == id {
		return
	}
	tmp := owner + ".tmp"
	if os.WriteFile(tmp, []byte(id), 0o600) == nil && os.Rename(tmp, owner) != nil {
		_ = os.Remove(tmp) // it stays unable to tell, as above
	}
}

// setToken writes a random token, unless another process got there first, and checkpoints so that it
// reaches the main file's header.
func (s *Store) setToken(ctx context.Context) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }() // back to the pool
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	var current int64
	err = conn.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&current)
	if err == nil && current == 0 {
		b := make([]byte, 4)
		if _, err = rand.Read(b); err == nil {
			// application_id is signed, and a value of 0 means none.
			token := int32(binary.BigEndian.Uint32(b)>>1) | 1
			_, err = conn.ExecContext(ctx, `PRAGMA application_id = `+strconv.Itoa(int(token)))
		}
	}
	if err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`) // the error is what matters
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// checkWALBelongs refuses a -wal with commits in it that came with a different database. A -wal says
// nothing of which database it belongs to, and SQLite would replay it into this one and damage it: from
// the SQLite docs, https://www.sqlite.org/howtocorrupt.html, "Mispairing database files and hot
// journals". This catches a database replaced by another in any way, and a -wal carried in with its
// owner file. It can't see a root with no owner file yet, an older copy of the same database with its
// later -wal, which has the same token, or a database and its owner file copied in together under a -wal
// that isn't theirs. A restore's own guarantee is that it leaves no -wal behind.
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
	token, err := fileToken(path)
	if err != nil || strconv.FormatUint(uint64(token), 10) == string(recorded) {
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
