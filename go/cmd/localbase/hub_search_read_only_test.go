package main

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// `models search` reads the root's saved Hugging Face token without writing the database (S14): it
// opens it read-only. It used to open it read-write, which migrates: on a database from before a
// table was added, a search added it. Here one of this build's tables is missing, as on such a
// database, and stays missing; the token is still read and sent; and the file is unchanged.
func TestModelsSearchReadsTheTokenWithoutWriting(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	t.Setenv("HF_TOKEN", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var auths []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer hub.Close()
	t.Setenv("HF_ENDPOINT", hub.URL)

	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key", "--hf-token", "hf_planted_search_token"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	db := filepath.Join(root, "local-base.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`DROP TABLE model_placements`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	tableThere := func() bool {
		t.Helper()
		ro, err := store.OpenReadOnly(db)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		var n int
		if err := ro.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'model_placements'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	sum := func() [32]byte {
		t.Helper()
		b, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()
	code, out := runApp(t, "--root", root, "models", "search", "qwen")
	// Nor does it say it read a copy: the read is incidental to a search (operator, #874), and the
	// Hub golden (TestCLIHubGolden, TS parity) has no such line.
	if code != 0 || strings.Contains(out, "Could not read this root's saved Hugging Face token") || strings.Contains(out, "Read a copy") {
		t.Errorf("search: %d\n%s", code, out)
	}
	mu.Lock()
	got := strings.Join(auths, ",")
	mu.Unlock()
	if !strings.Contains(got, "Bearer hf_planted_search_token") {
		t.Errorf("the hub was asked with %q, not the saved token", got)
	}
	if tableThere() {
		t.Error("search migrated the database: the missing table is back")
	}
	if sum() != before {
		t.Error("search changed the database")
	}
}
