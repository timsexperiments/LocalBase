package safefile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
)

// ErrNotAbsolute is RealDir given a relative directory.
var ErrNotAbsolute = errors.New("safefile: not an absolute directory")

// ErrNotRealDir is RealDir finding a component that is missing, not a directory, or a link.
var ErrNotRealDir = errors.New("safefile: not a real directory")

// RealDir checks that every component of the absolute directory dir is a real directory, not a
// symlink, and returns it, with macOS's /var, /tmp and /etc read as their /private targets.
func RealDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", ErrNotAbsolute
	}
	if goruntime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp", "/etc"} {
			if dir == alias || strings.HasPrefix(dir, alias+"/") {
				dir = "/private" + dir
				break
			}
		}
	}
	cur := "/"
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", ErrNotRealDir
		}
	}
	return cur, nil
}

// PublishError is where PublishNew failed: Step is "create" (the temporary file), "write" (writing
// or syncing it) or "link" (publishing it, most often because name is there already).
type PublishError struct {
	Step string
	Err  error
}

func (e *PublishError) Error() string { return "safefile: publish (" + e.Step + "): " + e.Err.Error() }
func (e *PublishError) Unwrap() error { return e.Err }

// PublishNew writes data to a new private file name in dir, which RealDir has checked, and never
// replaces one: a private temporary file in dir (tmpPrefix, random hex, ".tmp"), created without
// following a link, is written and synced, then hard-linked to name, and dir is synced. The
// temporary file is always removed.
func PublishNew(dir, name, tmpPrefix string, data []byte) error {
	b := make([]byte, 16)
	rand.Read(b)
	tmp := filepath.Join(dir, tmpPrefix+hex.EncodeToString(b)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return &PublishError{"create", err}
	}
	defer os.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &PublishError{"write", err}
	}
	if err := os.Link(tmp, filepath.Join(dir, name)); err != nil {
		return &PublishError{"link", err}
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
