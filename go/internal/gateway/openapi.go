package gateway

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/version"
)

// The API described for tools: DESIGN §30. The document is edited by hand, and
// openapi_test.go fails when it and the registered routes disagree.

//go:embed openapi.json
var openAPISource []byte

// openAPIVersionMark is info.version in the file; serving replaces it.
const openAPIVersionMark = "set-when-served"

var openAPIServed = sync.OnceValues(func() ([]byte, string) {
	body := bytes.Replace(openAPISource, []byte(`"version": "`+openAPIVersionMark+`"`), []byte(`"version": "`+version.Version+`"`), 1)
	sum := sha256.Sum256(body)
	return body, `"` + hex.EncodeToString(sum[:16]) + `"`
})

func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, r, "GET, HEAD")
		return
	}
	body, etag := openAPIServed()
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// routeMux records what is registered, so a test can compare the routes with
// the document: a ServeMux cannot list its own.
type routeMux struct {
	*http.ServeMux
	patterns []string
}

func newRouteMux() *routeMux { return &routeMux{ServeMux: http.NewServeMux()} }

func (m *routeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, handler)
}

func (m *routeMux) Handle(pattern string, handler http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, handler)
}
