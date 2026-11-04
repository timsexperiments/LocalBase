package lab

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// DatabaseCeiling is the most descriptors a settled gateway should hold on its database and
// sidecars, with the store in WAL: one on the database for each connection the pool has ever opened,
// up to its limit (they stay, even after the pool closes the connection), one on the -wal for each
// idle connection, one -shm between them, and the store's own held descriptor. They stay because a
// WAL connection holds a shared lock on the database for as long as it is attached (sqlite.org/
// walformat.html, 2.3.1), and SQLite won't close a file while any connection in the process holds a
// lock on it, since that would clear those locks; it keeps the descriptor to reuse on the next open
// (unixClose and findReusableFd in its source).
const DatabaseCeiling = store.MaxOpenConns + store.MaxIdleConns + 1 + store.HeldDescriptors

// DatabaseFiles is how many descriptors a process holds on a database and its sidecars.
type DatabaseFiles struct {
	DB      int `json:"db"`
	WAL     int `json:"wal"`
	SHM     int `json:"shm"`
	Journal int `json:"journal"`
}

// Total is all of them.
func (d DatabaseFiles) Total() int { return d.DB + d.WAL + d.SHM + d.Journal }

// DatabaseFiles counts what this gateway holds on its own database. Only its pid, only reading.
func (g *Gateway) DatabaseFiles() (DatabaseFiles, error) {
	return countDatabaseFiles(g.Cmd.Process.Pid, filepath.Join(g.Root, "local-base.db"))
}

func countDatabaseFiles(pid int, db string) (DatabaseFiles, error) {
	if real, err := filepath.EvalSymlinks(db); err == nil {
		db = real
	}
	names, err := openFiles(pid)
	if err != nil {
		return DatabaseFiles{}, err
	}
	var d DatabaseFiles
	for _, name := range names {
		switch strings.TrimSuffix(name, " (deleted)") {
		case db:
			d.DB++
		case db + "-wal":
			d.WAL++
		case db + "-shm":
			d.SHM++
		case db + "-journal":
			d.Journal++
		}
	}
	return d, nil
}

// openFiles names what a process has open: /proc on Linux, lsof elsewhere.
func openFiles(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			// A descriptor can close between the listing and this.
			if name, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
				names = append(names, name)
			}
		}
		return names, nil
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		lsof = "/usr/sbin/lsof"
	}
	out, err := exec.Command(lsof, "-n", "-P", "-a", "-p", strconv.Itoa(pid), "-d", "0-65535", "-Fn").Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || len(out) == 0 { // lsof also exits 1 with some files unreadable
			return nil, fmt.Errorf("lsof: %w", err)
		}
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			names = append(names, line[1:])
		}
	}
	return names, nil
}

// DescriptorProblem says what is wrong with a gateway's descriptors between two settled readings,
// or "". It has two parts. Everything but the database's files is held to slack, exactly, as before.
// The database's files fill as the pool does, which is not a leak, so they get a ceiling instead of
// a growth budget. A single budget wide enough for the pool let a leak of one descriptor a request
// through a 31-request test.
func DescriptorProblem(first, last Stats, slack, ceiling int) string {
	before, after := first.FDs-first.Files.Total(), last.FDs-last.Files.Total()
	if after-before > slack {
		return fmt.Sprintf("descriptors other than the database's went from %d to %d, more than the %d allowed", before, after, slack)
	}
	if t := last.Files.Total(); t > ceiling {
		return fmt.Sprintf("%d descriptors on the database's files (db %d, -wal %d, -shm %d, -journal %d), more than the ceiling of %d", t, last.Files.DB, last.Files.WAL, last.Files.SHM, last.Files.Journal, ceiling)
	}
	return ""
}
