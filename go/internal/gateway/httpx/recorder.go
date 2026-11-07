package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

type Recorder struct {
	http.ResponseWriter
	Status int
	Auth   *auth.Principal
	Body   *bodyGuard // nil: no body, or no connection
	// TimeoutAnswer: the 408 is being written, past discarding.
	TimeoutAnswer bool
	// errBody is the start of an error response, read only for the error
	// envelope's type and code (never its message).
	errBody []byte
	// Usage is what the handler knew, for the hourly totals (DESIGN §32).
	Usage UsageNote
	// writes sets the connection's write deadline, and writeStall is how far off. A recorder made
	// for something inside the gateway has neither.
	writes     *http.ResponseController
	writeStall time.Duration
	// SendFailed says the client didn't get the whole answer.
	SendFailed bool
}

// FinalStatus is what the request came to. A status is sent before the answer, so a client that
// went away or stopped reading partway through an answer is a 499, whatever was sent first. An error
// stays the error it was.
func (r *Recorder) FinalStatus() int {
	status := r.Status
	if status == 0 {
		status = 200
	}
	if r.SendFailed && status < 400 {
		return 499
	}
	return status
}

// SendsWithin gives each piece of the answer this long to leave. net/http clears the deadline when
// the answer has gone, so it doesn't reach the next one on the connection.
func (r *Recorder) SendsWithin(stall time.Duration) {
	r.writes, r.writeStall = http.NewResponseController(r.ResponseWriter), stall
}

// ArmWrite starts the deadline for what is about to be sent. Silence between writes costs nothing.
func (r *Recorder) ArmWrite() {
	if r.writes != nil {
		_ = r.writes.SetWriteDeadline(time.Now().Add(r.writeStall)) // Not every connection has one, and nothing depends on it.
	}
}

// send writes an answer in pieces, each with its own deadline, so that a client that has stopped
// reading is let go and a slow one isn't.
func (r *Recorder) send(b []byte) (int, error) {
	whole := b
	if r.writes == nil {
		return r.ResponseWriter.Write(b)
	}
	sent := 0
	defer func() { r.SendFailed = r.SendFailed || sent < len(whole) }()
	for {
		piece := b[:min(len(b), WritePiece)]
		r.ArmWrite()
		n, err := r.ResponseWriter.Write(piece)
		sent += n
		b = b[len(piece):]
		if err != nil || len(b) == 0 {
			return sent, err
		}
	}
}

const errSniffBytes = 8 << 10

// ErrorKind is the error envelope's type and code ("" when the response is
// not one).
func (r *Recorder) ErrorKind() (typ, code string) {
	var env struct {
		Error *struct {
			Type any `json:"type"`
			Code any `json:"code"`
		} `json:"error"`
	}
	if len(r.errBody) == 0 || json.Unmarshal(r.errBody, &env) != nil || env.Error == nil {
		return "", ""
	}
	if t, ok := env.Error.Type.(string); ok {
		typ = t
	}
	switch c := env.Error.Code.(type) {
	case string:
		code = c
	case float64:
		code = strconv.FormatFloat(c, 'f', -1, 64)
	}
	return typ, code
}

// Discarding reports a request whose body deadline passed before any answer
// started. Whatever the handler makes of the failed read, the answer is a 408.
func (r *Recorder) Discarding() bool {
	return r.Status == 0 && !r.TimeoutAnswer && r.Body != nil && r.Body.expired.Load()
}

func (r *Recorder) WriteHeader(code int) {
	if r.Discarding() {
		return
	}
	if r.Status == 0 {
		r.Status = code
		// A refusal does not wait for a body it has not read: the server would
		// otherwise try to read it before sending the answer.
		if code >= 400 && r.Body != nil && !r.Body.done.Load() {
			r.Header().Set("Connection", "close")
		}
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(b []byte) (int, error) {
	if r.Discarding() {
		return len(b), nil
	}
	if r.Status == 0 {
		r.Status = 200
	}
	if r.Status >= 400 && len(r.errBody) < errSniffBytes {
		r.errBody = append(r.errBody, b[:min(len(b), errSniffBytes-len(r.errBody))]...)
	}
	return r.send(b)
}

func (r *Recorder) Flush() { _ = r.FlushError() }

// FlushError sends what is buffered. A flush that doesn't get out before the write deadline is a
// failed send, as a short write is: net/http closes the connection after it, and the handler may see
// the client's cancel before it writes again, so nothing else would record it.
func (r *Recorder) FlushError() error {
	if r.Discarding() {
		return nil
	}
	if r.writes == nil {
		if f, ok := r.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	r.ArmWrite()
	err := r.writes.Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		r.SendFailed = true
	}
	return err
}

func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

type RecKey struct{}
