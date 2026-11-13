package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// --- changing the root ---------------------------------------------------------------------------

// journal says what a restore in progress is doing. It is read back by --undo, as input: every
// path is built from a constant list and these validated names.
type journal struct {
	Version int      `json:"version"`
	Before  string   `json:"before"`
	Staging string   `json:"staging"`
	Had     []string `json:"had"`
	NewRoot bool     `json:"newRoot"`
}

var beforeRe = regexp.MustCompile(`^\.before-restore-[0-9]{8}-[0-9]{6}(-[0-9]+)?$`)

func journalPath(dir string) string { return filepath.Join(dir, root.RestoreJournalName) }

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Done is what a finished restore left.
type Done struct {
	// BeforeDir holds the old files, which hold secrets too. It is empty for a new root.
	BeforeDir string `json:"beforeDir,omitempty"`
}

// Commit makes the staged backup the root's. It holds the operation lease and the gateway lock
// throughout, so no gateway can start midway. A failure puts everything back; a crash leaves the
// journal, and root.Open refuses until --undo.
func (s *Staged) Commit() (*Done, error) {
	s.release = func() {}
	defer s.Discard()
	lease, err := lease.AcquireOperation(s.target, "restore")
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	state, err := checkTarget(s.target, s.replace)
	if err != nil {
		return nil, err
	}
	if state.hasData && !s.hadData {
		return nil, refuse("This root changed while this waited: it holds settings now, and the report said it had none. Nothing was changed. Run this again to see what it would do now.")
	}
	r, err := root.Init(s.target)
	if err != nil {
		return nil, err
	}
	if st, _, _ := r.OwnerState(); st == "stale" {
		if err := r.ClearStale(); err != nil {
			return nil, err
		}
	}
	unlock, err := r.Lock("", 0)
	if err != nil {
		return nil, err
	}
	defer unlock()

	stamp := beforePrefix + time.Now().UTC().Format("20060102-150405")
	j := &journal{Version: 1, Before: stamp, NewRoot: state.newRoot}
	if filepath.Dir(s.dir) == filepath.Dir(r.Dir) {
		j.Staging = filepath.Base(s.dir)
	}
	for n := 1; exists(filepath.Join(r.Dir, j.Before)); n++ {
		j.Before = fmt.Sprintf("%s-%d", stamp, n)
	}
	for _, name := range replaceable {
		if exists(filepath.Join(r.Dir, name)) {
			j.Had = append(j.Had, name)
		}
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if err := safefile.WritePrivateAtomic(journalPath(r.Dir), raw); err != nil {
		return nil, err
	}
	if err := s.opts.syncDir(r.Dir); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}

	release, err := s.holdRoot(context.Background(), r.Dir, r.DB())
	if err != nil {
		return nil, s.stopped(r.Dir, j, err)
	}
	s.release = release
	defer release()
	// Taking the lock can change which working files there are, and the journal has to say what's here.
	j.Had = nil
	for _, name := range replaceable {
		if exists(filepath.Join(r.Dir, name)) {
			j.Had = append(j.Had, name)
		}
	}
	if raw, err = json.Marshal(j); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := safefile.WritePrivateAtomic(journalPath(r.Dir), raw); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := s.opts.syncDir(r.Dir); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}

	if err := s.apply(r.Dir, j); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := verify(r.Dir); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := s.opts.atStep("finish"); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	// Closing it deletes whatever journal is at its path, even with its own database moved away, so that
	// happens while this restore's journal still keeps every command out.
	release()
	if err := s.opts.atStep("released"); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := os.Remove(journalPath(r.Dir)); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	// It has finished. If this removal is lost in a power cut the journal comes back, the root
	// refuses to open, and the person is told to run the undo. Nothing is damaged.
	_ = s.opts.syncDir(r.Dir)
	done := &Done{}
	before := filepath.Join(r.Dir, j.Before)
	if os.Remove(before) != nil { // it holds the old files, or removing an empty one failed
		done.BeforeDir = before
	}
	return done, nil
}

// stopped ends a restore that hasn't moved anything. A refusal says why, in its own words, and anything
// else is a failure like any other.
func (s *Staged) stopped(dir string, j *journal, cause error) error {
	var refused *Refused
	if !errors.As(cause, &refused) {
		return s.failed(dir, j, cause)
	}
	// Only the journal: nothing moved, and whatever else is here may be another process's working files.
	if err := os.Remove(journalPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%v And its journal couldn't be removed (%w). Run: local-base --root '%s' restore --undo", cause, err, dir)
	}
	_ = s.opts.syncDir(dir) // If this removal is lost in a power cut the journal returns, and the undo is safe.
	return cause
}

func (s *Staged) failed(dir string, j *journal, cause error) error {
	if err := undo(dir, j, s.release, s.opts); err != nil {
		return fmt.Errorf("the restore failed (%v), and so did putting things back (%w). Run: local-base --root '%s' restore --undo", cause, err, dir)
	}
	return fmt.Errorf("the restore failed, and everything was put back: %w", cause)
}

func (s *Staged) apply(dir string, j *journal) error {
	before := filepath.Join(dir, j.Before)
	if err := s.opts.atStep("before"); err != nil {
		return err
	}
	if err := os.Mkdir(before, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(before, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
		return err
	}
	for _, name := range j.Had {
		if err := s.opts.atStep("aside " + name); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(before, name)); err != nil {
			return err
		}
	}
	// The database last: until it is here, nothing looks like a restored root.
	order := append(append([]string{}, replaceable[1:]...), replaceable[0])
	for _, name := range order {
		from := filepath.Join(s.dir, name)
		if !exists(from) {
			continue
		}
		if err := s.opts.atStep("in " + name); err != nil {
			return err
		}
		if err := os.Rename(from, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return s.opts.syncDir(dir)
}

// verify gives the restored root the checks a gateway gets.
func verify(dir string) error {
	if _, err := root.OpenForUndo(dir); err != nil {
		return err
	}
	if bad := obs.CheckRootFiles(dir); len(bad) > 0 {
		return bad[0]
	}
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }() // Only read from here.
	cfg, err := st.LoadConfig(context.Background())
	if errors.Is(err, store.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.Root != dir {
		return fmt.Errorf("the restored settings name %q as their root", cfg.Root)
	}
	return nil
}

// undo puts a root back. It only ever moves names on the constant list, and is safe to run again.
// release lets go of the root's database, for a restore that still holds it.
func undo(dir string, j *journal, release func(), o options) error {
	before := filepath.Join(dir, j.Before)
	// Nothing new goes in until everything old is aside, so before that, whatever is at a name the journal
	// doesn't list isn't this restore's. It may be another process's working files, with writes in them,
	// or a settings file a command just made, and it stays. This has to be worked out before anything goes
	// back, and it's the same on a second run that had finished removing what was new.
	allAside := exists(before)
	for _, name := range j.Had {
		allAside = allAside && exists(filepath.Join(before, name))
	}
	if allAside {
		for _, name := range replaceable {
			if contains(j.Had, name) {
				continue
			}
			if err := o.atStep("undo remove " + name); err != nil {
				return err
			}
			// It was never there, so anything at its name now is new.
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	// The database goes back last, as it goes in last: if this stops partway, a later run finds it still
	// aside, along with whatever else hasn't gone back yet.
	for _, name := range append(append([]string{}, replaceable[1:]...), replaceable[0]) {
		old, here := filepath.Join(before, name), filepath.Join(dir, name)
		if !exists(old) {
			continue // It hasn't moved, or it's already back.
		}
		if err := o.atStep("undo back " + name); err != nil {
			return err
		}
		// It was moved aside, so anything at its name now is new.
		if err := os.RemoveAll(here); err != nil {
			return err
		}
		if err := os.Rename(old, here); err != nil {
			return err
		}
	}
	if err := os.Remove(before); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if j.Staging != "" {
		os.RemoveAll(filepath.Join(filepath.Dir(dir), j.Staging))
	}
	// The moves reach the disk while the journal still exists, so a failure here can be run again.
	if err := o.syncDir(dir); err != nil {
		return err
	}
	// As in Commit: closing it deletes whatever journal is at its path, so that comes before anyone can start.
	release()
	if err := o.atStep("undo released"); err != nil {
		return err
	}
	if err := os.Remove(journalPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Everything is back. If this removal is lost in a power cut the journal returns, and running
	// the undo again is safe.
	_ = o.syncDir(dir)
	if j.NewRoot {
		// This made the root, so it goes too, if that leaves nothing. It is never removed
		// with its contents: a journal must not be able to delete a root.
		os.Remove(filepath.Join(dir, ".localbase-root.json"))
		os.RemoveAll(filepath.Join(dir, "runtime", "gateway.lock"))
		os.Remove(filepath.Join(dir, "runtime"))
		os.Remove(dir)
	}
	return nil
}

// readJournal treats the journal as input: it may have been left by a crash, or by someone else.
func readJournal(dir string) (*journal, error) {
	raw, err := safefile.ReadOwnedRegular(journalPath(dir))
	if err != nil {
		return nil, err
	}
	var j journal
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return nil, refuse("The restore journal can't be read: %v", err)
	}
	if j.Version != 1 || !beforeRe.MatchString(j.Before) {
		return nil, refuse("The restore journal names something a restore doesn't make: %q.", j.Before)
	}
	if j.Staging != "" && !temporaryRe.MatchString(j.Staging) {
		return nil, refuse("The restore journal names something a restore doesn't make: %q.", j.Staging)
	}
	for _, name := range j.Had {
		if !contains(replaceable, name) {
			return nil, refuse("The restore journal names something a restore doesn't touch: %q.", name)
		}
	}
	before := filepath.Join(dir, j.Before)
	if info, err := os.Lstat(before); err == nil {
		if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm() != 0o700 {
			return nil, refuse("%s is not a private directory of yours, so it won't be moved back.", before)
		}
		for _, name := range replaceable {
			if info, err := os.Lstat(filepath.Join(before, name)); err == nil && (info.Mode()&fs.ModeSymlink != 0 || !ownedByMe(info)) {
				return nil, refuse("%s is a link or belongs to someone else, so it won't be moved back.", filepath.Join(before, name))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if j.Staging != "" {
		if info, err := os.Lstat(filepath.Join(filepath.Dir(dir), j.Staging)); err == nil && (!info.IsDir() || !ownedByMe(info)) {
			j.Staging = "" // not ours to remove
		}
	}
	return &j, nil
}

// Undo puts back a root that a crash left half restored.
func Undo(targetDir string) error {
	if os.Getuid() == 0 {
		return refuse("Refusing to undo a restore as root. Run this as the user who owns the root.")
	}
	r, err := root.OpenForUndo(targetDir)
	if err != nil {
		return err
	}
	if !exists(journalPath(r.Dir)) {
		return refuse("There is no unfinished restore on %s.", r.Dir)
	}
	lease, err := lease.AcquireOperation(r.Dir, "undo restore")
	if err != nil {
		return err
	}
	defer lease.Release()
	j, err := readJournal(r.Dir)
	if err != nil {
		return err
	}
	switch state, _, detail := r.OwnerState(); state {
	case "active":
		return refuse("A process still holds the gateway lock on %s. Stop it first.", r.Dir)
	case "stale":
		if err := r.ClearStale(); err != nil {
			return err
		}
	case "missing":
	default:
		return refuse("The gateway lock on %s can't be read: %s", r.Dir, detail)
	}
	return undo(r.Dir, j, func() {}, options{})
}
