package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// Sidecars are the suffixes of every file that belongs with a database. Whatever replaces, removes or
// tightens a database does the same to these: a -wal left beside another database can damage it.
var Sidecars = []string{"-wal", "-shm", "-journal", ownerSuffix, ownerSuffix + ".tmp"}

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
	var token uint32
	err := readFile(path, func(f io.ReaderAt, _ int64) error {
		b := make([]byte, 4)
		if _, err := f.ReadAt(b, tokenOffset); errors.Is(err, io.EOF) {
			return nil // a new, empty file
		} else if err != nil {
			return err
		}
		token = binary.BigEndian.Uint32(b)
		return nil
	})
	return token, err
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
	walID := "-"
	if id, err := fileID(path + "-wal"); err == nil {
		walID = id
	}
	writeOwner(path, ownerRecord{token: token, wal: walID})
}

// ownerRecord is what the owner file says: this database's token, and which -wal is its own, or that
// it stopped cleanly and so has none. A token alone can't judge a -wal, which carries none: another
// database's -wal beside this one would pass.
type ownerRecord struct {
	token uint32
	wal   string // a file's device and inode, ownerCleanStop, or "-" if there's no telling
}

const ownerCleanStop = "stopped"

func (o ownerRecord) String() string {
	return strconv.FormatUint(uint64(o.token), 10) + " " + o.wal
}

func readOwner(path string) (ownerRecord, error) {
	b, err := os.ReadFile(path + ownerSuffix)
	if err != nil {
		return ownerRecord{}, err
	}
	token, wal, ok := strings.Cut(string(b), " ")
	n, err := strconv.ParseUint(token, 10, 32)
	if !ok || err != nil {
		return ownerRecord{}, fmt.Errorf("the owner file can't be read: %q", b)
	}
	return ownerRecord{uint32(n), wal}, nil
}

func writeOwner(path string, o ownerRecord) {
	owner := path + ownerSuffix
	if b, err := os.ReadFile(owner); err == nil && string(b) == o.String() {
		return
	}
	tmp := owner + ".tmp"
	if os.WriteFile(tmp, []byte(o.String()), 0o600) == nil && os.Rename(tmp, owner) != nil {
		_ = os.Remove(tmp) // it stays unable to tell, as in claimWAL
	}
}

// fileID says which file this is, whatever it's called.
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

// markCleanStop records that the last connection closed and took the -wal with it: any -wal with
// commits found beside this database from now on isn't its own.
func markCleanStop(path string) {
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		return // another process still has it open, or this can't tell
	}
	if token, err := fileToken(path); err == nil && token != 0 {
		writeOwner(path, ownerRecord{token: token, wal: ownerCleanStop})
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

// checkWALBelongs refuses a -wal with commits in it that isn't this database's. A -wal says nothing of
// which database it belongs to, and SQLite would replay it into this one and damage it: from the SQLite
// docs, https://www.sqlite.org/howtocorrupt.html, "Mispairing database files and hot journals". This
// catches a database replaced by another in any way, a -wal put beside a database that stopped cleanly,
// and one that replaced its own after a crash, as a copy does. It can't see a root with no owner file
// yet, an older copy of the same database put back under its own later -wal, or a different -wal that
// lands on the recorded inode number after a crash. A restore's own guarantee is that it leaves no -wal.
func checkWALBelongs(path string) error {
	wal := path + "-wal"
	info, err := os.Stat(wal)
	if err != nil || info.Size() == 0 {
		return nil
	}
	if _, err := os.Stat(path + ownerSuffix); err != nil {
		return nil // no record to compare, as on a root that hasn't opened with this build
	}
	refused := fmt.Errorf("%s holds writes that may belong to a different copy of this database, so LocalBase didn't open it: replaying them could replace or damage this database. If they don't belong here, move %[1]s somewhere safe and try again. If they do, as after copying a whole root, remove %[2]s and try again. Removing %[2]s is safe only then.", wal, path+ownerSuffix)
	recorded, err := readOwner(path)
	if err != nil {
		return refused
	}
	token, err := fileToken(path)
	if err != nil {
		return nil
	}
	if token != recorded.token {
		return refused
	}
	// After a clean stop the record names no file, so no -wal can match it, whatever inode it has.
	if recorded.wal != "-" {
		if id, err := fileID(wal); err == nil && id != recorded.wal {
			return refused
		}
	}
	return nil
}

// CheckWALBelongs is the check every open makes, for a caller that has to open a database its own way.
func CheckWALBelongs(path string) error { return checkWALBelongs(path) }

// HoldFile keeps this process's locks on the file at path safe from any raw read of it, for a caller that
// opens a database its own way, as a store does for itself. Call it before the first connection, and let go
// after the last has closed. It follows the file, not the path, and letGo can be called more than once.
func HoldFile(path string) (letGo func()) {
	h := holdFile(path)
	var once sync.Once
	return func() { once.Do(h.letGo) }
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
	return privateCopy(path)
}

// ReadablePath is the file a read of this database should open, by the same rule as OpenReadOnly, for a
// command that reads it several times and would otherwise copy it each time. done removes any copy.
func ReadablePath(path string) (read string, done func(), err error) {
	if err := checkWALBelongs(path); err != nil {
		return "", func() {}, err
	}
	stage, read, err := readablePath(path)
	if err != nil {
		return "", func() {}, err
	}
	return read, func() { removeStage(stage) }, nil
}

// PrivateCopy is a copy of this database and its -wal to read, for when the file itself can't be read,
// as with a -shm that can't be used. done removes it.
func PrivateCopy(path string) (read string, done func(), err error) {
	stage, read, err := privateCopy(path)
	if err != nil {
		return "", func() {}, err
	}
	return read, func() { removeStage(stage) }, nil
}

func privateCopy(path string) (stage, read string, err error) {
	// A copy of files someone is writing can be torn, so it's taken only with nobody holding the root.
	if root.InUse(filepath.Dir(path)) {
		return "", "", ErrInUse
	}
	parent := stageParent()
	for _, p := range stageParents() {
		sweepStages(p, time.Now())
	}
	stage, err = os.MkdirTemp(parent, stagePrefix+strconv.Itoa(os.Getpid())+"-"+stageTag(filepath.Dir(path))+"-") // 0700
	if err != nil {
		return "", "", err
	}
	holdStage(stage)
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

// stageParents is everywhere a copy could be, since the cache folder can come and go.
func stageParents() []string {
	parents := []string{}
	if cache, err := os.UserCacheDir(); err == nil {
		parents = append(parents, filepath.Join(cache, "local-base"))
	}
	return append(parents, os.TempDir())
}

// stageTag says which root a copy came from, so that one root's uninstall leaves another's alone.
func stageTag(rootDir string) string {
	dir, err := root.Canonical(rootDir)
	if err != nil {
		dir = filepath.Clean(rootDir)
	}
	return root.Hash(dir)[:16]
}

// stageLockName is a file in a copy's folder, locked for as long as its process holds the copy. The
// kernel lets go of a lock when a process ends in any way, and a new process given the same pid doesn't
// hold it, so this says a copy's owner has gone where a pid can't.
const stageLockName = ".owner"

// heldStages keeps each lock's file open until its copy is removed.
var heldStages sync.Map

func holdStage(stage string) {
	f, err := os.OpenFile(filepath.Join(stage, stageLockName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return // stageOwnerGone goes by the pid instead
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close() // the same
		return
	}
	heldStages.Store(stage, f)
}

// stageOwnerGone says the process holding a copy has ended, given the folder's name after the prefix. A
// copy with no lock file, from a process killed just as it began or on a filesystem without locks, goes
// by its pid, which a new process can reuse: then it counts as held until that process ends. Anything
// that can't be told counts as held.
func stageOwnerGone(path, name string) bool {
	pidText, _, _ := strings.Cut(name, "-")
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	f, err := os.OpenFile(filepath.Join(path, stageLockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	defer func() { _ = f.Close() }() // only read
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // closing lets go as well
		return true
	}
	return false
}

// isOwnStage says this entry is a real folder of this user's, with the name a copy has. A symlink with
// that name isn't one, and in a shared temp folder another user's isn't this user's to touch.
func isOwnStage(path string, e fs.DirEntry) (name string, ok bool) {
	name, ok = strings.CutPrefix(e.Name(), stagePrefix)
	if !ok || !e.IsDir() {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if st, isStat := info.Sys().(*syscall.Stat_t); !isStat || int(st.Uid) != os.Getuid() {
		return "", false
	}
	return name, true
}

// sweepStages removes copies a crash left: they can hold secrets. Only ones past stageMaxAge whose
// process has gone, named as this code names them.
func sweepStages(parent string, now time.Time) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(parent, e.Name())
		name, ok := isOwnStage(path, e)
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < stageMaxAge {
			continue
		}
		if stageOwnerGone(path, name) {
			_ = os.RemoveAll(path)
		}
	}
}

// ReadCopy is a private copy of a database, left by a read or still in use.
type ReadCopy struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ReadCopies finds this root's copies, of any age, in every place one could be. left are those whose
// process has gone. reading are those held by another process, which removes them itself. This
// process's own are in neither. Nothing here deletes anything.
func ReadCopies(rootDir string) (left, reading []ReadCopy) {
	tag := "-" + stageTag(rootDir) + "-"
	for _, parent := range stageParents() {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(parent, e.Name())
			name, ok := isOwnStage(path, e)
			if !ok || !strings.Contains(name, tag) {
				continue
			}
			pidText, _, _ := strings.Cut(name, "-")
			if pidText == strconv.Itoa(os.Getpid()) {
				if _, held := heldStages.Load(path); held {
					continue
				}
			}
			found := ReadCopy{Path: path, Bytes: stageBytes(path)}
			if stageOwnerGone(path, name) {
				left = append(left, found)
			} else {
				reading = append(reading, found)
			}
		}
	}
	return left, reading
}

func stageBytes(stage string) int64 {
	var total int64
	entries, err := os.ReadDir(stage)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// RemoveReadCopy removes a copy ReadCopies found left. It looks again first, so a copy that has come
// into use since stays. os.RemoveAll doesn't follow a symlink inside the folder.
func RemoveReadCopy(c ReadCopy) error {
	info, err := os.Lstat(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name, ok := strings.CutPrefix(filepath.Base(c.Path), stagePrefix)
	if !ok || !info.IsDir() || !stageOwnerGone(c.Path, name) {
		return fmt.Errorf("it isn't a copy whose process has gone")
	}
	return os.RemoveAll(c.Path)
}

// InWAL says whether a database's own header says it's in write-ahead logging.
func InWAL(path string) bool { return inWAL(path) }

func inWAL(path string) bool {
	found := false
	_ = readFile(path, func(f io.ReaderAt, _ int64) error { // no file reads as not in write-ahead logging
		header := make([]byte, 20)
		if _, err := f.ReadAt(header, 0); err != nil {
			return err
		}
		found = strings.HasPrefix(string(header), "SQLite format 3") && header[18] == walHeaderMode
		return nil
	})
	return found
}

// copyPrivate copies a database or its -wal. A -wal has no locks of this process's on it to lose, and
// going through readFile for both is simpler than telling them apart.
func copyPrivate(from, to string) error {
	return readFile(from, func(in io.ReaderAt, size int64) error {
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.NewSectionReader(in, 0, size)); err != nil {
			_ = out.Close() // the copy has already failed
			return err
		}
		return out.Close()
	})
}

// removeStage removes a private copy: it can hold secrets. A crash leaves it, private, for sweepStages.
func removeStage(stage string) {
	if stage == "" {
		return
	}
	_ = os.RemoveAll(stage)
	if f, held := heldStages.LoadAndDelete(stage); held {
		_ = f.(*os.File).Close() // nothing was written to it
	}
}
