package store

import (
	"io"
	"os"
	"sync"
)

// A process's locks on a file are cancelled when it closes any descriptor on that file, whatever opened
// it. A connection in write-ahead logging holds a shared lock on the database for as long as it lives,
// which is how another process knows it's attached, and SQLite thinks it still holds that lock, so it
// never takes it again. One os.Open and Close of the database by this process would quietly end that:
// from the SQLite docs, "How To Corrupt An SQLite Database File", section 2.2. So while a store has the
// database open, every raw read here shares one descriptor, which stays open until the last store on
// that file has closed. Nothing outside this file opens a database: TestOnlyThisFileOpensADatabase.
type heldFile struct {
	id   string
	f    *os.File
	refs int
}

var (
	heldFilesMu sync.Mutex
	// heldFiles is by device and inode, since locks follow the file: a database put at the same path
	// is another file. An inode can't be reused while its descriptor here keeps it alive.
	heldFiles = map[string]*heldFile{}
)

// holdFile keeps a descriptor on the file at path, until letGo. Call it before the first connection. A
// nil result is a file that can't be opened: SQLite will say so.
func holdFile(path string) *heldFile {
	heldFilesMu.Lock()
	defer heldFilesMu.Unlock()
	id, err := fileID(path)
	if err != nil {
		return nil
	}
	if h, ok := heldFiles[id]; ok {
		h.refs++
		return h
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	h := &heldFile{id: id, f: f, refs: 1}
	heldFiles[id] = h
	return h
}

// letGo ends a hold. The last one closes the descriptor, so it comes after the connections have closed.
func (h *heldFile) letGo() {
	if h == nil {
		return
	}
	heldFilesMu.Lock()
	defer heldFilesMu.Unlock()
	h.refs--
	if h.refs == 0 {
		delete(heldFiles, h.id)
		_ = h.f.Close() // only read
	}
}

// readFile gives read the file at path, through the held descriptor if a store has it open. read mustn't
// seek or close it. With no store holding the file, this process has no connection of a store on it, so
// a descriptor of its own is safe to close.
func readFile(path string, read func(io.ReaderAt, int64) error) error {
	heldFilesMu.Lock()
	defer heldFilesMu.Unlock()
	f, closeAfter, err := fileToRead(path)
	if err != nil {
		return err
	}
	if closeAfter {
		defer func() { _ = f.Close() }() // only read
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return read(f, info.Size())
}

func fileToRead(path string) (f *os.File, closeAfter bool, err error) {
	if id, err := fileID(path); err == nil {
		if h, ok := heldFiles[id]; ok {
			return h.f, false, nil
		}
	}
	f, err = os.Open(path)
	return f, true, err
}
