package acquire

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/install"
)

// What one import can cost (DESIGN §31.3). These are variables for tests.
var (
	// ImportDeadline bounds a whole import, however well it progresses. A slow
	// link can take a day over a very large model.
	ImportDeadline = 48 * time.Hour
	// maxUnknownSizeBytes bounds a file whose source does not say how big it is.
	maxUnknownSizeBytes int64 = 256 << 30
	// freeSpaceReserve stays free on the volume, so a download cannot stop the
	// database and logs from being written.
	freeSpaceReserve int64 = 2 << 30
	// freeSpaceEvery is how often a running download looks at the free space.
	freeSpaceEvery int64 = 64 << 20
	// maxPlanFiles bounds the files one import can fetch.
	maxPlanFiles = 1024
	// stagingMaxAge is how long an abandoned download is kept for resuming.
	stagingMaxAge = 7 * 24 * time.Hour
	// availableBytes is the space an unprivileged user can use at a path.
	availableBytes = install.FreeBytes
)

// Stable codes for what limits an import.
const (
	CodeNoSpace   = "import_no_space"
	CodeTooLarge  = "import_too_large"
	CodeTooMany   = "import_too_many_files"
	CodeCancelled = "import_cancelled"
	CodeTimedOut  = "import_timed_out"
)

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

func noSpace(need, usable int64) error {
	return &RefusedError{CodeNoSpace, fmt.Sprintf("There is not enough free disk space for this model: it needs %s, and %s can be used.", gib(need), gib(max(usable, 0)))}
}

// CheckSpace refuses a plan whose files do not fit, by what the source says their
// sizes are and what is already staged. A size is a claim, so a running download
// looks again as it goes.
func CheckSpace(rootDir string, p *Plan) error {
	free, err := availableBytes(filepath.Join(rootDir, "models"))
	if err != nil {
		return nil // no way to tell here; a running download looks again
	}
	staging := stagingDir(rootDir, p)
	var need int64
	for _, f := range p.Files {
		if f.SizeBytes <= 0 {
			continue
		}
		have := int64(0)
		if st, err := os.Stat(filepath.Join(staging, f.Filename)); err == nil {
			have = st.Size()
		} else if st, err := os.Stat(filepath.Join(staging, f.Filename+".partial")); err == nil {
			have = st.Size()
		}
		need += max(f.SizeBytes-have, 0)
	}
	if usable := free - freeSpaceReserve; need > usable {
		return noSpace(need, usable)
	}
	return nil
}

// budget stops a running download that would cost too much.
type budget struct {
	dir       string
	unknown   bool // the source did not say how big the file is
	limit     int64
	onHost    bool
	nextCheck int64
}

func newBudget(dir string, expected int64) *budget {
	return &budget{dir: dir, unknown: expected <= 0, limit: maxUnknownSizeBytes}
}

// budget is a plan's own limits, for one of its files.
func (p *Plan) budget(dir string, expected int64) *budget {
	b := newBudget(dir, expected)
	if p.MaxUnknownBytes > 0 {
		b.limit = p.MaxUnknownBytes
	}
	b.onHost = p.onHost
	return b
}

func tooLarge(limit int64, onHost bool) error {
	message := fmt.Sprintf("This source did not say how big the file is, and it passed the %s limit for that. Nothing was kept.", gib(limit))
	if onHost {
		message += " To allow more, run: local-base configure --import-max-unknown-gib <GiB>"
	} else {
		message += " Whoever runs this LocalBase can raise the limit."
	}
	return &RefusedError{CodeTooLarge, message}
}

func (b *budget) check(done int64) error {
	if b.unknown && done > b.limit {
		return tooLarge(b.limit, b.onHost)
	}
	if done < b.nextCheck {
		return nil
	}
	b.nextCheck = done + freeSpaceEvery
	if free, err := availableBytes(b.dir); err == nil && free < freeSpaceReserve {
		return noSpace(done, free-freeSpaceReserve+done)
	}
	return nil
}

// RemoveStaging discards what a plan has downloaded.
func RemoveStaging(rootDir string, p *Plan) error { return os.RemoveAll(stagingDir(rootDir, p)) }

// PruneStaging removes downloads nothing has touched for a while, except keep. A
// failed download is kept so it can resume, but not for ever.
func PruneStaging(rootDir, keep string) {
	base := filepath.Join(rootDir, "models", ".staging")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stagingMaxAge)
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if dir == keep || !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		young := false
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if fi, err := f.Info(); err == nil && fi.ModTime().After(cutoff) {
				young = true
			}
		}
		if !young {
			os.RemoveAll(dir)
		}
	}
}
