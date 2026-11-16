package audit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A temporary key is made with what was asked, expires in a day, and is audited when made and when
// ended, both with the purpose; once ended it is gone.
func TestTemporaryKey(t *testing.T) {
	ctx := context.Background()
	r, err := root.Init(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := New(r, io.Discard)
	k, raw, end, err := l.TemporaryKey(ctx, st, "a command (temporary)", []string{"models:read"}, "testing")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "a command (temporary)" || !slices.Equal(k.Scopes, []string{"models:read"}) || k.ExpiresAt == nil || raw == "" {
		t.Errorf("made %+v", k)
	}
	if err := end(); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range keys {
		if x.ID == k.ID {
			t.Errorf("still there: %+v", x)
		}
	}
	b, err := os.ReadFile(filepath.Join(r.LogsDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var made, ended int
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, k.ID) || !strings.Contains(line, `"purpose":"testing"`) {
			continue
		}
		switch {
		case strings.Contains(line, `"action":"create"`):
			made++
		case strings.Contains(line, `"action":"delete"`):
			ended++
		}
	}
	if made != 1 || ended != 1 {
		t.Errorf("audited: %d made, %d ended:\n%s", made, ended, b)
	}
}
