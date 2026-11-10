package browser

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The browser layer's tests that read its insides run against an Access whose API is a stub, not a
// gateway: what is tested is what Access answers itself. Their HTTP-only twins in gateway/local_test.go keep
// a whole gateway, so the two can't drift apart unnoticed.

// newAccessHarness serves an Access over a real socket, which is what the loopback rule reads, with
// local sign-in's pages, no browser sign-in and a scratch store. The API behind it answers 200 to
// what it is allowed: GET /_localbase/models, with its path and the names of the headers it got.
func newAccessHarness(t *testing.T) (*httptest.Server, *Access, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "localbase.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for name := range r.Header {
			names = append(names, name)
		}
		httpx.JSON(w, 200, map[string]any{"path": r.URL.Path, "headers": names})
	})
	a := NewAccess(Options{
		Handler: api,
		Store:   st,
		Log:     log,
		Pages:   gatewaytest.Pages{},
		Allowed: func(method, path string) bool { return method == "GET" && path == "/_localbase/models" },
		Caller:  func(*http.Request) *auth.Principal { return nil },
		LookupFailed: func(r *http.Request, err error) {
			auth.MarkLookupFailed(r.Context())
		},
	})
	ts := httptest.NewServer(http.HandlerFunc(a.UIAccess))
	t.Cleanup(ts.Close)
	return ts, a, st
}
