package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := waitGatewayIdle(&cli.Ctx{Stdout: io.Discard, Stderr: &stderr}, activityEnv(t, st), srv.URL, 0, func(time.Duration) {}); err != nil {
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
	if err := waitGatewayIdle(&cli.Ctx{Stdout: io.Discard, Stderr: &stderr}, activityEnv(t, st), srv.URL, 0, func(time.Duration) {}); err != nil {
		t.Fatalf("the answer was good, so the question should still succeed: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "temporary key") || !strings.Contains(got, "local-base keys revoke ") {
		t.Errorf("stderr should name the key and how to revoke it: %q", got)
	}
}

// A --wait that polls many times asks with one temporary key for the whole wait: one key created
// and deleted, not one per poll (review 01-M2).
func TestWaitingForAnIdleGatewayUsesOneKey(t *testing.T) {
	st := activityStore(t)
	polls, keys := 0, map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		keys[r.Header.Get("Authorization")] = true
		busy := polls <= 3
		_, _ = w.Write([]byte(fmt.Sprintf(`{"host":{"activity":{"busy":%v}}}`, busy)))
	}))
	defer srv.Close()
	var stderr bytes.Buffer
	if err := waitGatewayIdle(&cli.Ctx{Stdout: io.Discard, Stderr: &stderr}, activityEnv(t, st), srv.URL, time.Minute, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if polls != 4 || len(keys) != 1 {
		t.Errorf("%d polls with %d different keys; want 4 polls with one key", polls, len(keys))
	}
	if left, err := st.ListAPIKeys(context.Background()); err != nil || len(left) != 0 {
		t.Errorf("keys left: %v %v", left, err)
	}
}

// activityEnv is the env the wait runs with: a store, a scratch root, and that root's audit log.
func activityEnv(t *testing.T, st *store.Store) *env {
	r := &root.Root{Dir: t.TempDir()}
	return &env{Store: st, Root: r, audit: newAuditLog(r, nil)}
}
