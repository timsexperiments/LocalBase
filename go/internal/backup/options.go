package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"syscall"
	"time"
)

// options are the points in this package a test reaches into. Its zero value is how the package
// runs, and the exported functions pass that, so nothing here can be set from outside.
type options struct {
	busyWait       time.Duration // (tests) how long a step keeps trying a busy database; 0 is busyWaitLimit
	readBusyMillis int           // (tests) how long one read waits on SQLite, in ms; 0 is readBusyMillisLimit

	step       func(string) error                   // (tests) runs at each named step of a commit and its undo: an error is a failure, a panic a crash
	dirSync    func(*os.File) error                 // (tests) nil is (*os.File).Sync on a directory: a failing disk can't be arranged any other way
	settle     func(context.Context, *sql.DB) error // (tests) nil is settleJournalMode, which no real file has been found to fail
	targetRead func()                               // (tests) runs just after the comparison's first read of the target: a writer arriving then
	link       func(oldname, newname string) error  // (tests) nil is os.Link: a file system without links takes placeFile's copying path
	chmod      func(*os.File, os.FileMode) error    // (tests) nil is (*os.File).Chmod
}

// busyWaitFor is busyWaitLimit, or the shorter wait a test set.
func (o options) busyWaitFor() time.Duration {
	if o.busyWait > 0 {
		return o.busyWait
	}
	return busyWaitLimit
}

// readBusyMillisFor is readBusyMillisLimit, or the shorter wait a test set.
func (o options) readBusyMillisFor() int {
	if o.readBusyMillis > 0 {
		return o.readBusyMillis
	}
	return readBusyMillisLimit
}

func (o options) atStep(name string) error {
	if o.step != nil {
		return o.step(name)
	}
	return nil
}

// syncDir makes the renames in a directory last through a power cut. A file system that can't do
// that for a directory says so with EINVAL or ENOTSUP, and there is nothing more to wait for there.
func (o options) syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	sync := (*os.File).Sync
	if o.dirSync != nil {
		sync = o.dirSync
	}
	if err := sync(d); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (o options) settleJournal(ctx context.Context, db *sql.DB) error {
	if o.settle != nil {
		return o.settle(ctx, db)
	}
	return settleJournalMode(ctx, db)
}

func (o options) targetReadDone() {
	if o.targetRead != nil {
		o.targetRead()
	}
}

func (o options) linkFile(oldname, newname string) error {
	if o.link != nil {
		return o.link(oldname, newname)
	}
	return os.Link(oldname, newname)
}

func (o options) chmodFile(f *os.File, mode os.FileMode) error {
	if o.chmod != nil {
		return o.chmod(f, mode)
	}
	return f.Chmod(mode)
}
