package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func activityStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestActivityKeyIsGoneAfterAsking(t *testing.T) {
	st := activityStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"host":{"activity":{"busy":false}}}`))
	}))
	defer srv.Close()
	var stderr bytes.Buffer
	if _, _, err := gatewayActivity(&cli.Ctx{Stdout: io.Discard, Stderr: &stderr}, &env{Store: st, Root: &root.Root{Dir: t.TempDir()}}, srv.URL); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || stderr.Len() != 0 {
		t.Errorf("keys left: %d, stderr: %q", len(keys), stderr.String())
	}
}

// A working key left behind is worth a line even though it lasts one day and only reads, and the
// line has to say which key, or nobody can revoke it.
func TestActivityKeyThatCannotBeDeletedIsReported(t *testing.T) {
	st := activityStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		st.Close() // so the delete after this request fails
		_, _ = w.Write([]byte(`{"host":{"activity":{"busy":false}}}`))
	}))
	defer srv.Close()
	var stderr bytes.Buffer
	if _, _, err := gatewayActivity(&cli.Ctx{Stdout: io.Discard, Stderr: &stderr}, &env{Store: st, Root: &root.Root{Dir: t.TempDir()}}, srv.URL); err != nil {
		t.Fatalf("the answer was good, so the question should still succeed: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "temporary key") || !strings.Contains(got, "local-base keys revoke ") {
		t.Errorf("stderr should name the key and how to revoke it: %q", got)
	}
}
