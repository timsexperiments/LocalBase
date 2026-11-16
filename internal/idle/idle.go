// Package idle ends HTTP downloads that stop sending. TCP keepalive does not catch a
// peer that stays connected and sends nothing, and a download has no overall limit
// that fits a large file, so without this one silent server blocks an install forever.
package idle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// DefaultTimeout is how long a download may go without a byte.
const DefaultTimeout = 60 * time.Second

// Timeout is the limit in force: LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS, or the default.
func Timeout() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return DefaultTimeout
}

// Error says a server stopped sending. It is a network timeout, and so retryable.
type Error struct{ After time.Duration }

func (e *Error) Error() string {
	return fmt.Sprintf("the server sent nothing for %s", e.After)
}

// Timeout and Temporary make it a net.Error.
func (e *Error) Timeout() bool   { return true }
func (e *Error) Temporary() bool { return true }

// Do sends req through client with the limit applied to the wait for the response
// and to each read of its body. Only silence counts: time spent writing what arrived
// does not. Closing the body ends the request as usual.
func Do(client *http.Client, req *http.Request, limit time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(limit, func() { cancel(&Error{After: limit}) })
	resp, err := client.Do(req.WithContext(ctx))
	timer.Stop()
	if err != nil {
		defer cancel(nil)
		return nil, cause(ctx, err)
	}
	resp.Body = &body{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timer: timer, limit: limit}
	return resp, nil
}

type body struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	limit  time.Duration
}

func (b *body) Read(p []byte) (int, error) {
	b.timer.Reset(b.limit)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	if err != nil && err != io.EOF {
		err = cause(b.ctx, err)
	}
	return n, err
}

func (b *body) Close() error {
	b.timer.Stop()
	defer b.cancel(nil)
	return b.ReadCloser.Close()
}

// cause prefers the stall over the cancellation it produced.
func cause(ctx context.Context, err error) error {
	var stalled *Error
	if c := context.Cause(ctx); errors.As(c, &stalled) {
		return stalled
	}
	return err
}
