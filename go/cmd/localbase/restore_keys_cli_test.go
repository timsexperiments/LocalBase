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
	want = "Keys going back to what this backup holds, because you named them:\n  " + key.ID + " (laptop): rotated\n  The secret in this backup works for these, and any they have now stops working.\n"
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
	want := "Keys held back, because they can do more than make requests:\n  " + admin.ID + " (admin): keys:manage\n  To bring one back: --bring-back-key <id>\n"
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
			"Keys going back to what this backup holds, because you named them:\n  key_a (laptop): rotated, permissions changed\n  The secret in this backup works for these, and any they have now stops working.\n"},
		{func(r *backup.Report) { r.KeysLost = bare },
			"Keys made since this backup would stop working:\n  key_a (laptop)\n"},
		{func(r *backup.Report) {
			r.KeysActive = []backup.KeyChange{{ID: "key_a", Name: "laptop", Why: "inference:chat"}}
		},
			"API keys in this backup that would work:\n  key_a (laptop): inference:chat\n"},
		{func(r *backup.Report) {
			r.KeysHeldBack = []backup.KeyChange{{ID: "key_a", Name: "admin", Why: "access:manage, keys:manage"}}
		},
			"Keys held back, because they can do more than make requests:\n  key_a (admin): access:manage, keys:manage\n  To bring one back: --bring-back-key <id>\n"},
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
