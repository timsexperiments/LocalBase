package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strconv"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// CallResult is an in-process API response.
type CallResult struct {
	Status int
	Header http.Header
	Body   []byte
}

type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferWriter) Header() http.Header { return b.header }
func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(p)
}

// Call runs an API route in-process as the signed-in browser principal, the
// way /app/api forwards a browser request: the same validation, permissions,
// admission and errors as an API client gets. The UI's Lab and voices pages
// use it instead of a JSON client.
func (s *Server) Call(ctx context.Context, p *auth.Principal, method, path, contentType string, body []byte) CallResult {
	r, _ := http.NewRequestWithContext(context.WithValue(ctx, browserPrincipalKey{}, p), method, "http://localbase.internal"+path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:0"
	r.RequestURI = path
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	w := &bufferWriter{header: http.Header{}}
	s.ServeHTTP(w, r)
	if w.status == 0 {
		w.status = 200
	}
	return CallResult{Status: w.status, Header: w.header, Body: w.body.Bytes()}
}
