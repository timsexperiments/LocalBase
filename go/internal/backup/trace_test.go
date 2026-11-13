package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// traced records, in order, each step a restore takes, each directory it syncs, and the checks a stage
// makes of the root it would replace. Directories are relative to base.
type traced struct {
	base string
	got  []string
	// fail is the step that fails, if any.
	fail string
}

func (tr *traced) step(s string) error {
	tr.got = append(tr.got, s)
	if s == tr.fail {
		return errors.New("injected")
	}
	return nil
}

func (tr *traced) sync(d *os.File) error {
	rel, err := filepath.Rel(tr.base, d.Name())
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = "outside: " + d.Name()
	}
	tr.got = append(tr.got, "sync "+rel)
	return d.Sync()
}

func (tr *traced) settle(ctx context.Context, db *sql.DB) error {
	tr.got = append(tr.got, "settle")
	return settleJournalMode(ctx, db)
}

func (tr *traced) targetRead() { tr.got = append(tr.got, "target read") }

// each puts prefix before each name.
func each(prefix string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}
	return out
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// traceCommit commits s with its steps and directory syncs traced.
func traceCommit(t *testing.T, s *Staged, tr *traced) error {
	t.Helper()
	wasStep, wasSync := stepHook, dirSync
	stepHook, dirSync = tr.step, tr.sync
	defer func() { stepHook, dirSync = wasStep, wasSync }()
	_, err := s.Commit()
	return err
}

// tracePlace places archive at out with its directory syncs traced, and with links or without.
func tracePlace(t *testing.T, archive, out string, links bool, tr *traced) error {
	t.Helper()
	wasLink, wasSync := linkFile, dirSync
	if !links {
		linkFile = func(from, to string) error { return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV} }
	}
	dirSync = tr.sync
	defer func() { linkFile, dirSync = wasLink, wasSync }()
	return placeFile(archive, out)
}

// traceStage stages file over target with the stage's checks of the target traced.
func traceStage(t *testing.T, file, target string, replace bool, tr *traced) (*Staged, error) {
	t.Helper()
	wasSettle, wasRead := settleJournal, targetReadHook
	settleJournal, targetReadHook = tr.settle, tr.targetRead
	defer func() { settleJournal, targetReadHook = wasSettle, wasRead }()
	return Stage(context.Background(), file, target, replace)
}

// What a restore does, and in what order: each step, each directory synced, and the checks a stage makes
// of the root it replaces. Moving the seams that see these mustn't change any of them.
func TestARestoresStepsAndSyncsInOrder(t *testing.T) {
	// What a root with data had, set aside in the order it's listed; then the backup's files moved in,
	// with the database last.
	had := []string{"local-base.db", "local-base.db-journal", "local-base.db-wal-owner", "ui-access.json",
		"email-delivery.json", "credentials", "catalog.d", "backends.d", "voices"}
	in := []string{"local-base.db-wal-owner", "ui-access.json", "email-delivery.json", "credentials", "catalog.d",
		"backends.d", "voices", "local-base.db"}
	// The journal's sync, the root held and compared, the journal again, and the directory for what's set aside.
	held := []string{"sync root", "hold", "compared", "switched", "sync root", "before"}
	moved := cat(held, each("aside ", had...), each("in ", in...), []string{"sync root"})
	for _, c := range []struct {
		name string
		run  func(t *testing.T, tr *traced) error
		fail string
		want []string
		// says is how the commit ends: "" for success, or what its error starts with.
		says string
	}{
		{"over a root with data", func(t *testing.T, tr *traced) error {
			r, _ := newRoot(t, tr.base)
			return traceCommit(t, stageOver(t, r), tr)
		}, "", cat(moved, []string{"finish", "released", "sync root"}), ""},
		{"on a new root", func(t *testing.T, tr *traced) error {
			r, _ := newRoot(t, dir(t))
			res := create(t, r, false)
			s, err := Stage(context.Background(), res.Output, filepath.Join(tr.base, "root"), false)
			if err != nil {
				t.Fatal(err)
			}
			return traceCommit(t, s, tr)
		}, "", cat([]string{"sync root", "hold", "sync root", "before"}, each("in ", in...),
			[]string{"sync root", "finish", "released", "sync root"}), ""},
		{"failing at the end, and put back", func(t *testing.T, tr *traced) error {
			r, _ := newRoot(t, tr.base)
			return traceCommit(t, stageOver(t, r), tr)
		}, "finish", cat(moved, []string{"finish"},
			// What the root didn't have is removed, then what it had goes back, the database last.
			each("undo remove ", "local-base.db-wal", "local-base.db-shm", "local-base.db-wal-owner.tmp"),
			each("undo back ", append(append([]string{}, had[1:]...), had[0])...),
			[]string{"sync root", "undo released", "sync root"}), "the restore failed, and everything was put back: injected"},
		{"stopped before anything moved", func(t *testing.T, tr *traced) error {
			r, _ := newRoot(t, tr.base)
			s := stageOver(t, r)
			changeRoot(t, r, `UPDATE api_keys SET name = 'another'`)
			return traceCommit(t, s, tr)
		}, "", []string{"sync root", "hold", "compared", "sync root"}, "This root changed while this waited: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &traced{base: dir(t), fail: c.fail}
			err := c.run(t, tr)
			if (c.says == "") != (err == nil) || (err != nil && !strings.HasPrefix(err.Error(), c.says)) {
				t.Errorf("commit: %v, want %q", err, c.says)
			}
			if !reflect.DeepEqual(tr.got, c.want) {
				t.Errorf("got %#v\nwant %#v", tr.got, c.want)
			}
		})
	}
}

// Placing a backup syncs its directory, linked or copied.
func TestAPlacedBackupsSyncsInOrder(t *testing.T) {
	for _, links := range []bool{true, false} {
		archive, out := placeFixture(t)
		tr := &traced{base: filepath.Dir(filepath.Dir(out))}
		if err := tracePlace(t, archive, out, links, tr); err != nil {
			t.Fatal(err)
		}
		if want := []string{"sync out"}; !reflect.DeepEqual(tr.got, want) {
			t.Errorf("links %v: got %#v, want %#v", links, tr.got, want)
		}
	}
}

// A stage over a root with data reads the root it would replace, then settles the backup's journal mode;
// a stage over a new root has nothing to read.
func TestAStagesChecksInOrder(t *testing.T) {
	for _, c := range []struct {
		name    string
		replace bool
		want    []string
	}{
		{"over a root with data", true, []string{"target read", "settle"}},
		{"on a new root", false, []string{"settle"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			res := create(t, r, false)
			target := r.Dir
			if !c.replace {
				target = filepath.Join(dir(t), "root")
			}
			tr := &traced{}
			s, err := traceStage(t, res.Output, target, c.replace, tr)
			if err != nil {
				t.Fatal(err)
			}
			s.Discard()
			if !reflect.DeepEqual(tr.got, c.want) {
				t.Errorf("got %#v\nwant %#v", tr.got, c.want)
			}
		})
	}
}
