package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
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
	code := func(f importForm) string {
		t.Helper()
		_, err := u.resolve(req, f)
		var refused *acquire.RefusedError
		if !errors.As(err, &refused) {
			return ""
		}
		// The page shows this text to whoever is signed in, perhaps on a phone.
		if strings.Contains(refused.Message, "configure") || strings.Contains(refused.Message, "allow-") {
			t.Errorf("the message names a setting: %s", refused.Message)
		}
		return refused.Code
	}
	if c := code(source); c != acquire.CodePrivateAddress || reached.Load() != 0 {
		t.Errorf("a private address: %q, reached %d", c, reached.Load())
	}
	path := importForm{Source: filepath.Join(t.TempDir(), "file.gguf"), ID: "x", Task: "chat"}
	if c := code(path); c != acquire.CodeLocalPath {
		t.Errorf("a local path: %q", c)
	}
	if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	u.resolve(req, source)
	if reached.Load() != 1 {
		t.Errorf("with the opt-in: reached %d", reached.Load())
	}
	if c := code(path); c != acquire.CodeLocalPath {
		t.Errorf("a local path with only the private opt-in: %q", c)
	}
	if err := st.SetSetting(context.Background(), store.LocalPathImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	if c := code(path); c != "" {
		t.Errorf("a local path with its opt-in is judged as a file: %q", c)
	}
}
