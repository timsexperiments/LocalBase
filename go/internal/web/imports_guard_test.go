package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/netguard"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

func TestImportPageKeepsToPublicAddresses(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer srv.Close()
	u := &UI{d: Deps{Store: st, RootDir: t.TempDir()}}
	req := httptest.NewRequest(http.MethodPost, "/app/models/import/resolve", nil)
	source := importForm{Source: srv.URL + "/m.gguf", AllowHTTP: true, ID: "x", Task: "chat", Confirm: true}
	if _, err := u.resolve(req, source); !netguard.IsBlocked(err) || reached.Load() != 0 {
		t.Errorf("a private address: %v, reached %d", err, reached.Load())
	}
	path := importForm{Source: filepath.Join(t.TempDir(), "file.gguf"), ID: "x", Task: "chat"}
	if _, err := u.resolve(req, path); err == nil || !strings.Contains(err.Error(), "allow-private-imports") {
		t.Errorf("a local path: %v", err)
	}
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	u.resolve(req, source)
	if reached.Load() != 1 {
		t.Errorf("with the opt-in: reached %d", reached.Load())
	}
	if _, err := u.resolve(req, path); err == nil || strings.Contains(err.Error(), "allow-private-imports") {
		t.Errorf("a local path with the opt-in is judged as a file: %v", err)
	}
}
