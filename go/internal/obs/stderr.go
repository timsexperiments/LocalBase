package obs

import (
	"os"
	"path/filepath"
)

// The managed macOS gateway's stderr goes to one private, size-bounded file
// (#357): launchd never rotates job output.

const maxServiceStderr = 1 << 20

func stderrPaths(rootDir string) (string, string) {
	dir := filepath.Join(rootDir, "logs")
	return filepath.Join(dir, "service-stderr.log"), filepath.Join(dir, "service-stderr.log.1")
}

// BoundServiceStderr moves the newest 1 MiB of an oversized capture to the
// .1 archive and truncates the active file (launchd appends from zero).
func BoundServiceStderr(rootDir string) error {
	active, archive := stderrPaths(rootDir)
	f, err := os.OpenFile(active, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() <= maxServiceStderr {
		return err
	}
	tail := make([]byte, maxServiceStderr)
	if _, err := f.ReadAt(tail, st.Size()-maxServiceStderr); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(active), ".service-stderr."+uuid()+".tmp")
	if err := os.WriteFile(tmp, tail, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, archive); err != nil {
		os.Remove(tmp)
		return err
	}
	return f.Truncate(0)
}

// StderrSummary describes the captured stderr without its contents.
type StderrSummary struct {
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	ModifiedAt   string `json:"modifiedAt"`
	ArchiveBytes int64  `json:"archiveBytes"`
}

// InspectServiceStderr returns nil when nothing was captured.
func InspectServiceStderr(rootDir string) *StderrSummary {
	active, archive := stderrPaths(rootDir)
	a, aErr := os.Lstat(active)
	b, bErr := os.Lstat(archive)
	s := &StderrSummary{Path: active}
	if aErr == nil && a.Mode().IsRegular() {
		s.Bytes = a.Size()
	}
	if bErr == nil && b.Mode().IsRegular() {
		s.ArchiveBytes = b.Size()
	}
	if s.Bytes == 0 && s.ArchiveBytes == 0 {
		return nil
	}
	mod := b
	if s.Bytes > 0 {
		mod = a
	}
	s.ModifiedAt = mod.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")
	return s
}
