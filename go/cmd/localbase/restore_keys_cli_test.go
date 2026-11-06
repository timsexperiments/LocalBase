package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func secretWorks(t *testing.T, root, secret string) bool {
	t.Helper()
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ResolveAPIKey(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	return got != nil
}

// The real commands, from a key rotated by `keys rotate` to a restore with and without the flag.
func TestARestoreLeavesARotatedKeyAloneUnlessItIsNamed(t *testing.T) {
	root := noAuthRoot(t)
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	key, old, err := st.CreateAPIKey(context.Background(), "laptop", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	if code, out := runApp(t, "--root", root, "keys", "rotate", key.ID); code != 0 {
		t.Fatalf("rotate: %s", out)
	}
	st, err = store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := st.APIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out := runApp(t, "--root", root, "restore", "--replace", "--dry-run", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("dry run: %s", out)
	}
	want := "Keys changed since this backup stay as they are on this root:\n  " + key.ID + " (laptop): rotated\n  To take this backup's version of one: --bring-back-key <id>\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !strings.Contains(out, backup.ChangedKeyLine) {
		t.Errorf("the rule isn't there:\n%s", out)
	}
	if strings.Contains(out, rotated.KeyHash) || strings.Contains(out, old) {
		t.Errorf("a secret or its hash is in the report")
	}

	code, out = runApp(t, "--root", root, "restore", "--replace", "--yes", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	if secretWorks(t, root, old) {
		t.Error("the old secret came back")
	}

	code, out = runApp(t, "--root", root, "restore", "--replace", "--yes", "--no-port-probe", "--bring-back-key", key.ID, file)
	if code != 0 {
		t.Fatalf("restore naming it: %s", out)
	}
	want = "Keys going back to what this backup holds, because you named them:\n  " + key.ID + " (laptop): rotated\n  This backup's secret for each of these will work. The secret each has now will stop working.\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !secretWorks(t, root, old) {
		t.Error("the secret the backup holds doesn't work")
	}
	events, err := os.ReadFile(filepath.Join(root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"keysbroughtback":"`+key.ID+`"`) {
		t.Errorf("the record of the restore doesn't name the key:\n%s", events)
	}
}

// The real commands again: an administrator's key onto a new root stays out until it's named, while an
// ordinary one comes.
func TestARestoreHoldsBackAnAdminKeyUnlessItIsNamed(t *testing.T) {
	root := noAuthRoot(t)
	st, err := store.Open(filepath.Join(root, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	admin, adminSecret, err := st.CreateAPIKey(context.Background(), "admin", []string{"inference:chat", "keys:manage"})
	if err != nil {
		t.Fatal(err)
	}
	_, ordinarySecret, err := st.CreateAPIKey(context.Background(), "laptop", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}

	held := filepath.Join(t.TempDir(), "held")
	code, out := runApp(t, "--root", held, "restore", "--yes", "--no-port-probe", file)
	if code != 0 {
		t.Fatalf("restore: %s", out)
	}
	want := "Keys held back, because they can do more than make requests. Bring back only one you recognise:\n  " + admin.ID + " (admin): keys:manage\n    To bring it back: --bring-back-key " + admin.ID + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	real, err := filepath.EvalSymlinks(filepath.Dir(held))
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	want = "  A key held back can still come back from this backup: local-base --root '" + filepath.Join(real, "held") + "' restore --replace --bring-back-key " + admin.ID + " '" + abs + "'\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !strings.Contains(out, backup.HeldKeyLine) {
		t.Errorf("the rule isn't there:\n%s", out)
	}
	if secretWorks(t, held, adminSecret) {
		t.Error("the administrator's key works")
	}
	if !secretWorks(t, held, ordinarySecret) {
		t.Error("the ordinary key doesn't work")
	}
	events, err := os.ReadFile(filepath.Join(held, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"keysheldback":1`) {
		t.Errorf("the record of the restore doesn't count it:\n%s", events)
	}

	// What that line says, after the restore has finished.
	code, out = runApp(t, "--root", held, "restore", "--replace", "--yes", "--no-port-probe", "--bring-back-key", admin.ID, file)
	if code != 0 {
		t.Fatalf("restore naming it afterwards: %s", out)
	}
	if !secretWorks(t, held, adminSecret) {
		t.Error("the key didn't come back afterwards")
	}
	if !secretWorks(t, held, ordinarySecret) {
		t.Error("the ordinary key stopped working afterwards")
	}

	named := filepath.Join(t.TempDir(), "named")
	code, out = runApp(t, "--root", named, "restore", "--yes", "--no-port-probe", "--bring-back-key", admin.ID, file)
	if code != 0 {
		t.Fatalf("restore naming it: %s", out)
	}
	want = "Keys going back to what this backup holds, because you named them:\n  " + admin.ID + " (admin): can do keys:manage\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
	if !secretWorks(t, named, adminSecret) {
		t.Error("the key that was named doesn't work")
	}
}

func TestARestoreRefusesToNameAKeyItWouldNotChange(t *testing.T) {
	root := noAuthRoot(t)
	file := filepath.Join(t.TempDir(), "b.lbbackup")
	if code, out := runApp(t, "--root", root, "backup", "--output", file); code != 0 {
		t.Fatalf("backup: %s", out)
	}
	code, out := runApp(t, "--root", root, "restore", "--replace", "--dry-run", "--no-port-probe", "--bring-back-key", "key_nothing", file)
	if code == 0 || !strings.Contains(out, "--bring-back-key key_nothing: there's nothing to bring back.") {
		t.Errorf("%d: %s", code, out)
	}
}

func keyList(n int) []backup.KeyChange {
	out := make([]backup.KeyChange, n)
	for i := range out {
		out[i] = backup.KeyChange{ID: fmt.Sprintf("key_%03d", i), Name: "laptop", Why: "rotated"}
	}
	return out
}

func keyLines(rep *backup.Report) string {
	var out, errb bytes.Buffer
	printRestoreReport(&cli.Ctx{Stdout: &out, Stderr: &errb}, rep)
	return out.String() + errb.String()
}

func emptyReport() *backup.Report {
	return &backup.Report{Manifest: &backup.Manifest{}}
}

// Each line a person reads, exactly, since these are what they decide by.
func TestTheKeyLinesRead(t *testing.T) {
	one := []backup.KeyChange{{ID: "key_a", Name: "laptop", Why: "rotated, permissions changed"}}
	bare := []backup.KeyChange{{ID: "key_a", Name: "laptop"}}
	for _, c := range []struct {
		set  func(*backup.Report)
		want string
	}{
		{func(r *backup.Report) { r.KeysKept = one },
			"Keys changed since this backup stay as they are on this root:\n  key_a (laptop): rotated, permissions changed\n  To take this backup's version of one: --bring-back-key <id>\n"},
		{func(r *backup.Report) { r.KeysLeftOut = bare },
			"Keys in this backup that this root doesn't have stay out:\n  key_a (laptop)\n  To bring one back: --bring-back-key <id>\n"},
		{func(r *backup.Report) { r.KeysBroughtBack = one },
			"Keys going back to what this backup holds, because you named them:\n  key_a (laptop): rotated, permissions changed\n  This backup's secret for each of these will work. The secret each has now will stop working.\n"},
		{func(r *backup.Report) { r.KeysLost = bare },
			"Keys made since this backup would stop working:\n  key_a (laptop)\n"},
		{func(r *backup.Report) {
			r.KeysActive = []backup.KeyChange{{ID: "key_a", Name: "laptop", Why: "inference:chat"}}
		},
			"API keys in this backup that would work:\n  key_a (laptop): inference:chat\n"},
		{func(r *backup.Report) {
			r.KeysHeldBack = []backup.KeyChange{{ID: "key_a", Name: "admin", Why: "access:manage, keys:manage"}, {ID: "key_b", Name: "other", Why: "keys:read"}}
		},
			"Keys held back, because they can do more than make requests. Bring back only one you recognise:\n  key_a (admin): access:manage, keys:manage\n    To bring it back: --bring-back-key key_a\n  key_b (other): keys:read\n    To bring it back: --bring-back-key key_b\n"},
	} {
		rep := emptyReport()
		c.set(rep)
		if got := keyLines(rep); !strings.Contains(got, c.want) {
			t.Errorf("no %q in:\n%s", c.want, got)
		}
	}
}

// With nothing to say, none of those headings print.
func TestNoKeyLinesWhenNoKeysDiffer(t *testing.T) {
	got := keyLines(emptyReport())
	for _, line := range []string{"Keys changed", "Keys in this backup", "Keys going back", "Keys made since", "API keys in this backup", "Keys held back", "--bring-back-key"} {
		if strings.Contains(got, line) {
			t.Errorf("%q in:\n%s", line, got)
		}
	}
}

// A long list doesn't bury what comes after it, and says how many it left out and how to see them.
func TestALongKeyListSaysHowManyMore(t *testing.T) {
	rep := emptyReport()
	rep.KeysKept = keyList(maxGrantLines + 2)
	got := keyLines(rep)
	if strings.Count(got, "(laptop): rotated") != maxGrantLines {
		t.Errorf("%d lines", strings.Count(got, "(laptop): rotated"))
	}
	if !strings.Contains(got, "  and 2 more. All of them: restore --dry-run --json\n") {
		t.Errorf("no count in:\n%s", got)
	}
	if !strings.Contains(got, "--bring-back-key <id>") {
		t.Errorf("the way to take one went with the list:\n%s", got)
	}
	rep.KeysKept = keyList(maxGrantLines)
	if got := keyLines(rep); strings.Contains(got, "more. All of them") {
		t.Errorf("a list that fits says there's more:\n%s", got)
	}
}

// An id comes from the file, and these lines are made to be pasted, so one a shell would act on, or take
// for an option, isn't offered. The key is still listed, so it can be read and typed by someone who
// wants it.
func TestAnIDAShellWouldActOnIsNotOfferedForPasting(t *testing.T) {
	for _, id := range []string{"key;touch x", "$(touch x)", "`touch x`", "key x", "-x", "--replace", "key|x", "key&x", "key>x", "key'x", "key\"x", "key\\x", "key\nx", "key_\u0430"} {
		t.Run(id, func(t *testing.T) {
			rep := emptyReport()
			rep.Target = "/a/root"
			rep.KeysHeldBack = []backup.KeyChange{{ID: id, Name: "admin", Why: "keys:manage"}}
			var out bytes.Buffer
			c := &cli.Ctx{Stdout: &out, Stderr: &out}
			printRestoreReport(c, rep)
			printRestoreNext(c, rep, &backup.Done{})
			got := out.String()
			if strings.Count(got, "--bring-back-key <id>") != 2 {
				t.Errorf("no form to fill in, in:\n%s", got)
			}
			for _, line := range strings.Split(got, "\n") {
				if rest, ok := strings.CutPrefix(line[strings.Index(line, "--bring-back-key")+1:], "-bring-back-key "); strings.Contains(line, "--bring-back-key") && ok && !strings.HasPrefix(rest, "<id>") {
					t.Errorf("offered for pasting: %q", line)
				}
			}
		})
	}
	for _, id := range []string{"key_0f3c9a52-1d4e-4b7a-9c11-2f6e8d7a1b00", "a.b:c-d_e"} {
		if got := bringBackFlag(id); got != "--bring-back-key "+id {
			t.Errorf("%q: %q", id, got)
		}
	}
}

// A path is one word in a line made to be pasted, whatever it holds.
func TestAPathStaysOneWord(t *testing.T) {
	if got := shellQuoted("/a/it's here"); got != `'/a/it'\''s here'` {
		t.Errorf("%s", got)
	}
	rep := emptyReport()
	rep.Target = "/a/it's"
	rep.KeysHeldBack = []backup.KeyChange{{ID: "key_a", Name: "admin"}}
	var out bytes.Buffer
	c := &cli.Ctx{Stdout: &out, Stderr: &out, Args: []string{"/b/that's.lbbackup"}}
	printRestoreNext(c, rep, &backup.Done{BeforeDir: "/a/it's.before"})
	got := out.String()
	for _, want := range []string{
		`Start it: local-base --root '/a/it'\''s' start`,
		`restore --replace --bring-back-key key_a '/b/that'\''s.lbbackup'`,
		`rm -rf '/a/it'\''s.before'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in:\n%s", want, got)
		}
	}
}

// That line may be pasted from another folder later, so a backup named by a relative path is given in
// full.
func TestTheBackupIsGivenInFull(t *testing.T) {
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := backupArgument(&cli.Ctx{Args: []string{"some.lbbackup"}})
	if want := shellQuoted(filepath.Join(here, "some.lbbackup")); got != want {
		t.Errorf("%s, want %s", got, want)
	}
	if got := backupArgument(&cli.Ctx{}); got != "<this backup>" {
		t.Errorf("with no file: %s", got)
	}
}

// With no key held back, nothing about bringing one back is said afterwards.
func TestNoLineAfterwardsWhenNoKeyIsHeldBack(t *testing.T) {
	var out bytes.Buffer
	printRestoreNext(&cli.Ctx{Stdout: &out, Stderr: &out}, emptyReport(), &backup.Done{})
	if strings.Contains(out.String(), "bring-back-key") {
		t.Errorf("%s", out.String())
	}
}

// Several held back: which one is someone's choice, so the line leaves the id to fill in.
func TestTheLineAfterwardsLeavesTheChoiceOfSeveralToThem(t *testing.T) {
	rep := emptyReport()
	rep.Target = "/a/root"
	rep.KeysHeldBack = []backup.KeyChange{{ID: "key_a"}, {ID: "key_b"}}
	var out bytes.Buffer
	printRestoreNext(&cli.Ctx{Stdout: &out, Stderr: &out}, rep, &backup.Done{})
	if !strings.Contains(out.String(), "restore --replace --bring-back-key <id> <this backup>") {
		t.Errorf("%s", out.String())
	}
}

// A long list of held-back keys says how many more there are, as the other lists do.
func TestALongHeldBackListSaysHowManyMore(t *testing.T) {
	rep := emptyReport()
	rep.KeysHeldBack = keyList(maxGrantLines + 2)
	got := keyLines(rep)
	if n := strings.Count(got, "To bring it back:"); n != maxGrantLines {
		t.Errorf("%d lines", n)
	}
	if !strings.Contains(got, "  and 2 more. All of them: restore --dry-run --json\n") {
		t.Errorf("no count in:\n%s", got)
	}
	rep.KeysHeldBack = keyList(maxGrantLines)
	if got := keyLines(rep); strings.Contains(got, "more. All of them") {
		t.Errorf("a list that fits says there's more:\n%s", got)
	}
}

// An id is what a person types back, so it can't hide what it is in another alphabet. A name is what
// they called it, and keeps its letters, since the id beside it says which key it is.
func TestAKeyIDIsShownForWhatItIs(t *testing.T) {
	const lookalike = "key_\u0430"
	rep := emptyReport()
	rep.KeysLeftOut = []backup.KeyChange{{ID: lookalike, Name: "port\u00e1til"}}
	got := keyLines(rep)
	if strings.Contains(got, lookalike) {
		t.Errorf("the id printed as it stands:\n%s", got)
	}
	if !strings.Contains(got, `key_\u0430`) {
		t.Errorf("the id isn't escaped:\n%s", got)
	}
	if !strings.Contains(got, "(port\u00e1til)") {
		t.Errorf("the name lost its letters:\n%s", got)
	}
}
