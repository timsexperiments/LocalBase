package httpx

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Limits bounds what one request can cost the gateway (DESIGN §31.1). A caller
// who has not authenticated gets a short deadline for its whole body. Once it has
// authenticated, an upload only has to keep moving, so a phone on a slow link can
// finish. Zero values take the defaults.
type Limits struct {
	// ReadHeader is how long a request may take to send its headers.
	ReadHeader time.Duration
	// Idle is how long a keep-alive connection may wait for its next request. Keep it
	// above 90 s, cloudflared's default keep-alive to its origin: below that, the
	// gateway closes connections the tunnel still expects to reuse.
	Idle time.Duration
	// MaxHeaderBytes bounds a request's headers.
	MaxHeaderBytes int
	// PreAuthBody is how long a request may take to send its body until it has
	// authenticated.
	PreAuthBody time.Duration
	// UploadStall is how long an authenticated upload may go without progress.
	UploadStall time.Duration
	// UploadTotal bounds an authenticated upload however well it progresses.
	UploadTotal time.Duration
	// RefusedDrain and RefusedDrainBytes bound what is read of a refused request's
	// body before answering.
	RefusedDrain      time.Duration
	RefusedDrainBytes int64
	// WriteStall is how long each piece of an answer may take to leave. A client that has stopped
	// reading keeps what its request holds, such as a generation, until this passes.
	WriteStall time.Duration
}

// WritePiece is how much of an answer has one deadline. At a minute for each, a client has to read
// about 1 KiB/s to keep up, however long the answer is.
const WritePiece = 64 << 10

// DefaultLimits are the limits of a gateway that sets none.
var DefaultLimits = Limits{
	ReadHeader:     30 * time.Second,
	Idle:           2 * time.Minute,
	MaxHeaderBytes: 64 << 10,
	PreAuthBody:    30 * time.Second,
	// A 25 MiB upload at 0.5 Mbit/s takes about 7 minutes.
	UploadStall:       2 * time.Minute,
	UploadTotal:       time.Hour,
	RefusedDrain:      2 * time.Second,
	RefusedDrainBytes: 1 << 20,
	WriteStall:        time.Minute,
}

func (l Limits) WithDefaults() Limits {
	d := DefaultLimits
	if l.ReadHeader > 0 {
		d.ReadHeader = l.ReadHeader
	}
	if l.Idle > 0 {
		d.Idle = l.Idle
	}
	if l.MaxHeaderBytes > 0 {
		d.MaxHeaderBytes = l.MaxHeaderBytes
	}
	if l.PreAuthBody > 0 {
		d.PreAuthBody = l.PreAuthBody
	}
	if l.UploadStall > 0 {
		d.UploadStall = l.UploadStall
	}
	if l.UploadTotal > 0 {
		d.UploadTotal = l.UploadTotal
	}
	if l.RefusedDrain > 0 {
		d.RefusedDrain = l.RefusedDrain
	}
	if l.RefusedDrainBytes > 0 {
		d.RefusedDrainBytes = l.RefusedDrainBytes
	}
	if l.WriteStall > 0 {
		d.WriteStall = l.WriteStall
	}
	return d
}

// Apply sets the server-wide part of the limits on an HTTP server.
func (l Limits) Apply(h *http.Server) {
	l = l.WithDefaults()
	h.ReadHeaderTimeout, h.IdleTimeout, h.MaxHeaderBytes = l.ReadHeader, l.Idle, l.MaxHeaderBytes
}

// bodyGuard is a request body whose read deadline follows the request: short until
// the caller authenticates, then moving with each read. net/http clears the deadline
// when the body ends, and no deadline may be set after that: the server's background
// read would turn its expiry into a cancelled request, ending a long response.
type bodyGuard struct {
	io.ReadCloser
	rc       *http.ResponseController
	limits   Limits
	started  time.Time
	authed   atomic.Bool
	draining atomic.Bool
	touched  atomic.Bool // something has read from the body
	done     atomic.Bool // the body has ended
	expired  atomic.Bool // its deadline passed while a handler was reading it
}

// GuardBody starts a request's deadline. It returns nil when there is no body, or no
// connection to set a deadline on (an in-process request).
func GuardBody(l Limits, w http.ResponseWriter, r *http.Request) *bodyGuard {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	rc := http.NewResponseController(w)
	now := time.Now()
	if rc.SetReadDeadline(now.Add(l.PreAuthBody)) != nil {
		return nil
	}
	return &bodyGuard{ReadCloser: r.Body, rc: rc, limits: l, started: now}
}

func (b *bodyGuard) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.touched.Store(true)
	switch {
	case errors.Is(err, io.EOF):
		b.done.Store(true)
	case errors.Is(err, os.ErrDeadlineExceeded) && !b.draining.Load():
		b.expired.Store(true)
	case n > 0 && b.authed.Load() && !b.draining.Load():
		b.extend()
	}
	return n, err
}

func (b *bodyGuard) extend() {
	deadline := time.Now().Add(b.limits.UploadStall)
	if end := b.started.Add(b.limits.UploadTotal); deadline.After(end) {
		deadline = end
	}
	_ = b.rc.SetReadDeadline(deadline) // GuardBody proved deadlines work here; if this fails, the earlier, shorter deadline stays
}

// Authenticated gives the caller the longer budget.
func (b *bodyGuard) Authenticated() {
	if b == nil || b.done.Load() || b.authed.Swap(true) {
		return
	}
	b.extend()
}

// MarkAuthenticated gives the request's caller the longer budget.
func MarkAuthenticated(r *http.Request) {
	if rec, ok := r.Context().Value(RecKey{}).(*Recorder); ok {
		rec.Body.Authenticated()
	}
}

// drainRefused reads a bounded part of what is left of a refused request's body, so
// a client still sending a small body reads the answer at a request boundary. A body
// that does not end within the bounds costs no more: the connection closes after the
// answer.
func drainRefused(w http.ResponseWriter, r *http.Request) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return
	}
	limits := DefaultLimits
	rec, _ := r.Context().Value(RecKey{}).(*Recorder)
	var g *bodyGuard
	if rec != nil {
		g = rec.Body
	}
	if g != nil {
		limits = g.limits
		if g.done.Load() {
			return
		}
		if strings.EqualFold(r.Header.Get("Expect"), "100-continue") && !g.touched.Load() {
			// The client is waiting to be told to send: nothing to read.
			w.Header().Set("Connection", "close")
			return
		}
		g.draining.Store(true)
		_ = g.rc.SetReadDeadline(time.Now().Add(limits.RefusedDrain)) // if this fails, the deadline already set still bounds the read, and the drain is capped in bytes
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, limits.RefusedDrainBytes+1))
	if err != nil || n > limits.RefusedDrainBytes {
		w.Header().Set("Connection", "close")
	}
}

// RequestTimedOut answers a request whose body stopped arriving. It is best effort:
// the connection may already be dead. A client that went away is a 499 instead, from
// the handler that noticed.
func RequestTimedOut(w http.ResponseWriter) {
	JSON(w, http.StatusRequestTimeout, map[string]any{"error": APIError{Message: "The request body did not arrive in time.",
		Type: "invalid_request_error", Code: "request_timeout"}})
}
