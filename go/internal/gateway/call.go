package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// callBodyLimit is the most of a response an in-process call holds, as a run's media result.
var callBodyLimit = runMaxResult

// errCallTooLarge tells a handler its in-process caller stopped reading.
var errCallTooLarge = errors.New("the response is over what an in-app call holds")

type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int  // the most it holds; 0 is no limit
	over   bool // past limit: what was kept is dropped, and nothing more is
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
	if b.over || (b.limit > 0 && b.body.Len()+len(p) > b.limit) {
		b.over = true
		b.body = bytes.Buffer{}
		return 0, errCallTooLarge
	}
	return b.body.Write(p)
}

// Call runs an API route in-process as the signed-in browser principal, the
// way /app/api forwards a browser request: the same validation, permissions,
// admission and errors as an API client gets. The UI's Lab and voices pages
// use it instead of a JSON client.
func (s *Server) Call(ctx context.Context, p *auth.Principal, method, path, contentType string, body []byte) CallResult {
	r, _ := http.NewRequestWithContext(context.WithValue(ctx, browserPrincipalKey{}, p), method, "http://localbase.internal"+path, bytes.NewReader(body))
	r.Host = "localhost" // in-process: this machine, whatever the URL says
	r.RemoteAddr = "127.0.0.1:0"
	r.RequestURI = path
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	w := &bufferWriter{header: http.Header{}, limit: callBodyLimit}
	s.ServeHTTP(w, r)
	if w.over {
		over := &bufferWriter{header: http.Header{}}
		upstreamFailure(over, r, fmt.Sprintf("The response is larger than %s, more than an in-app call can hold.", sizeText(callBodyLimit)))
		return CallResult{Status: over.status, Header: over.header, Body: over.body.Bytes()}
	}
	if w.status == 0 {
		w.status = 200
	}
	return CallResult{Status: w.status, Header: w.header, Body: w.body.Bytes()}
}
