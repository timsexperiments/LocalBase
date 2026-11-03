package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

var testBody = []byte("the model's bytes, all of them")

func specFor(url string, body []byte) *catalog.Spec {
	sum := sha256.Sum256(body)
	return &catalog.Spec{ModelID: "reason-test", Kind: "stt", Source: url + "/org/repo", RepositoryRevision: "abc",
		Artifacts: []catalog.Artifact{{SourcePath: "m.bin", Filename: "m.bin", ExpectedSizeBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Role: "primary"}}}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// Each reason through a real download against a fake server, except where noted.
func TestInstallFailureReasons(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
	}
	ok := func(w http.ResponseWriter, r *http.Request) { w.Write(testBody) }
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		opts    func(*Options)
		ctx     func() (context.Context, context.CancelFunc)
		spec    func(*catalog.Spec)
		want    string
	}{
		{name: "401", handler: status(401), want: ReasonGated},
		{name: "403", handler: status(403), want: ReasonGated},
		{name: "404", handler: status(404), want: ReasonNotFound},
		{name: "410", handler: status(410), want: ReasonNotFound},
		{name: "500", handler: status(500), want: ReasonNetwork},
		{name: "503", handler: status(503), want: ReasonNetwork},
		{name: "429", handler: status(429), want: ReasonNetwork},
		{name: "504", handler: status(504), want: ReasonTimeout},
		{name: "418 is not a known cause", handler: status(418), want: ReasonUnknown},
		{name: "cut off mid-body", want: ReasonNetwork, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(testBody)))
			w.Write(testBody[:4])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}},
		{name: "ended early with no length", want: ReasonNetwork, handler: func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			w.Write(testBody[:4])
		}},
		{name: "an HTML page with status 200", want: ReasonChecksumMismatch, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "11")
			w.Write([]byte("<html></html>"[:11]))
		}},
		{name: "right size, wrong bytes", want: ReasonChecksumMismatch, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Write(make([]byte, len(testBody)))
		}},
		{name: "too many bytes", want: ReasonChecksumMismatch, handler: func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			w.Write(append(append([]byte{}, testBody...), 'x'))
		}},
		{name: "not enough free space before starting", handler: ok, want: ReasonDiskFull, opts: func(o *Options) {
			o.Available = func(string) (int64, error) { return 1, nil }
		}},
		{name: "the disk fills while writing", handler: ok, want: ReasonDiskFull, opts: func(o *Options) {
			o.wrapWriter = func(io.Writer) io.Writer {
				return failingWriter{&os.PathError{Op: "write", Path: "m", Err: syscall.ENOSPC}}
			}
		}},
		{name: "a quota runs out while writing", handler: ok, want: ReasonDiskFull, opts: func(o *Options) {
			o.wrapWriter = func(io.Writer) io.Writer {
				return failingWriter{&os.PathError{Op: "write", Path: "m", Err: syscall.EDQUOT}}
			}
		}},
		{name: "another write error is not a full disk", handler: ok, want: ReasonUnknown, opts: func(o *Options) {
			o.wrapWriter = func(io.Writer) io.Writer {
				return failingWriter{&os.PathError{Op: "write", Path: "m", Err: syscall.EIO}}
			}
		}},
		{name: "cancelled", want: ReasonInterrupted, ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, handler: ok},
		{name: "deadline passed", want: ReasonTimeout, ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Millisecond)
		}, handler: func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}},
		{name: "the client's own timeout", want: ReasonTimeout, opts: func(o *Options) {
			o.Client = &http.Client{Timeout: 30 * time.Millisecond}
		}, handler: func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			ctx := context.Background()
			if c.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = c.ctx()
				defer cancel()
			}
			o := Options{Available: func(string) (int64, error) { return 1 << 40, nil }}
			if c.opts != nil {
				c.opts(&o)
			}
			_, err := Model(ctx, specFor(srv.URL, testBody), filepath.Join(t.TempDir(), "models"), o)
			if err == nil {
				t.Fatal("no error")
			}
			if got := Reason(err); got != c.want {
				t.Errorf("reason %q, want %q: %v", got, c.want, err)
			}
		})
	}
}

// A connection that is refused: nothing is listening.
func TestInstallReasonRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	_, err = Model(context.Background(), specFor(url, testBody), filepath.Join(t.TempDir(), "m"), Options{Available: func(string) (int64, error) { return 1 << 40, nil }})
	if got := Reason(err); got != ReasonNetwork {
		t.Errorf("%q: %v", got, err)
	}
}

// The name-gated sentence hides its cause from the text, not from the reason.
func TestInstallReasonThroughGatedMessage(t *testing.T) {
	err := downloadError("https://huggingface.co/google/gemma-x/m", "", &HTTPStatusError{403, "Forbidden"})
	if Reason(err) != ReasonGated {
		t.Errorf("%q", Reason(err))
	}
	if got := err.Error(); got == "" || !errors.As(err, new(*messageError)) {
		t.Errorf("%q", got)
	}
}

func TestReasonEdges(t *testing.T) {
	if Reason(nil) != "" {
		t.Error("nil should have no reason")
	}
	if Reason(errors.New("something else")) != ReasonUnknown {
		t.Error("an unrecognized error should be unknown")
	}
	// The sentences existing callers read did not change.
	if got := (&HTTPStatusError{404, "Not Found"}).Error(); got != "HTTP 404 Not Found" {
		t.Errorf("%q", got)
	}
}
