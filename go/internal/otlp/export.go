package otlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Export limits (TS otel.ts).
const (
	maxQueue      = 2048
	maxBatch      = 256
	exportDelay   = time.Second
	exportTimeout = 5 * time.Second
	closeDeadline = 5 * time.Second
	maxAttempts   = 2
	retryBase     = 250 * time.Millisecond
)

// Diagnostic reports exporter problems (severity info|warn|error). The
// caller writes it to the local log only, never back through the exporter.
type Diagnostic func(severity, message string, attrs map[string]any)

// Runtime is an active exporter: a tracer and a log sink. A nil *Runtime is
// the disabled one; every method accepts it.
type Runtime struct {
	cfg      *obs.OtelConfig
	version  string
	resource buf
	scope    buf
	spans    *batcher[*Span]
	logs     *batcher[logRecord]
	failures *Reporter
	diag     Diagnostic
	client   *http.Client
	closed   chan struct{}
	once     sync.Once
}

// New starts exporting, or returns nil when the configuration is disabled.
func New(cfg *obs.OtelConfig, version string, diag Diagnostic) *Runtime {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	if diag == nil {
		diag = func(string, string, map[string]any) {}
	}
	rt := &Runtime{cfg: cfg, version: version, diag: diag, failures: NewReporter(diag, time.Now), closed: make(chan struct{}),
		client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed")
		}}}
	rt.resource = encodeKVs(1, []attr{{"service.name", "local-base"}, {"service.namespace", "localbase"},
		{"service.version", version}})
	rt.resource.uintAlways(2, 0)
	rt.scope.str(1, "local-base")
	rt.scope.str(2, version)
	if cfg.TracesEndpoint != "" {
		h := merge(cfg.Headers, cfg.TracesHeaders)
		rt.spans = newBatcher(func(items []*Span) error {
			return rt.send(cfg.TracesEndpoint, h, rt.encodeSpans(items))
		}, "traces", rt.failures)
	}
	if cfg.LogsEndpoint != "" {
		h := merge(cfg.Headers, cfg.LogsHeaders)
		rt.logs = newBatcher(func(items []logRecord) error {
			return rt.send(cfg.LogsEndpoint, h, rt.encodeLogs(items))
		}, "logs", rt.failures)
	}
	return rt
}

// Enabled reports whether anything is exported.
func (rt *Runtime) Enabled() bool { return rt != nil }

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// Shutdown exports what is queued, within the close deadline, then reports
// outages that no summary covered yet.
func (rt *Runtime) Shutdown() {
	if rt == nil {
		return
	}
	rt.once.Do(func() {
		done := make(chan struct{})
		go func() {
			var wg sync.WaitGroup
			for _, b := range []interface{ close() }{rt.spans, rt.logs} {
				if b == nil || isNilBatcher(b) {
					continue
				}
				wg.Add(1)
				go func() { defer wg.Done(); b.close() }()
			}
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeDeadline):
			rt.diag("warn", "OpenTelemetry shutdown deadline exceeded.", map[string]any{"deadlineMs": closeDeadline.Milliseconds()})
			close(rt.closed) // aborts in-flight requests and retry waits
			<-done
		}
		rt.failures.Flush()
	})
}

func isNilBatcher(b interface{ close() }) bool {
	switch x := b.(type) {
	case *batcher[*Span]:
		return x == nil
	case *batcher[logRecord]:
		return x == nil
	}
	return false
}

// --- batching -------------------------------------------------------------------

// batcher is the SDK batch processor: a bounded queue (newest dropped when
// full), batches of at most maxBatch sent at least every exportDelay, one
// export at a time.
type batcher[T any] struct {
	mu       sync.Mutex
	queue    []T
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	export   func([]T) error
	signal   string
	failures *Reporter
}

func newBatcher[T any](export func([]T) error, signal string, failures *Reporter) *batcher[T] {
	b := &batcher[T]{wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), export: export,
		signal: signal, failures: failures}
	go b.run()
	return b
}

func (b *batcher[T]) add(item T) {
	if b == nil {
		return
	}
	b.mu.Lock()
	select {
	case <-b.stop:
		b.mu.Unlock()
		return
	default:
	}
	if len(b.queue) >= maxQueue {
		b.mu.Unlock()
		return
	}
	b.queue = append(b.queue, item)
	full := len(b.queue) >= maxBatch
	b.mu.Unlock()
	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

func (b *batcher[T]) take() []T {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(b.queue), maxBatch)
	items := append([]T(nil), b.queue[:n]...)
	b.queue = b.queue[n:]
	return items
}

func (b *batcher[T]) flushOne() bool {
	items := b.take()
	if len(items) == 0 {
		return false
	}
	if err := b.export(items); err != nil {
		b.failures.Failure(b.signal, Report{Signal: b.signal, Severity: "warn", Message: "OTLP export failed.",
			Detail: err.Error(), Items: len(items), Attrs: map[string]any{"batchSize": len(items)}})
	} else {
		b.failures.Recovered(b.signal)
	}
	return true
}

func (b *batcher[T]) run() {
	defer close(b.done)
	t := time.NewTicker(exportDelay)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			for b.flushOne() {
			}
			return
		case <-t.C:
		case <-b.wake:
		}
		b.flushOne()
	}
}

func (b *batcher[T]) close() {
	b.mu.Lock()
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	b.mu.Unlock()
	<-b.done
}

// --- sending --------------------------------------------------------------------

var retryable = map[int]bool{429: true, 502: true, 503: true, 504: true}

// send posts one batch: two attempts within a five-second deadline,
// retrying 429/502/503/504 and transport errors (Retry-After honoured when
// it fits). Failure text never includes the endpoint, headers or body.
func (rt *Runtime) send(endpoint string, headers map[string]string, body []byte) error {
	deadline := time.Now().Add(exportTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	go func() {
		select {
		case <-rt.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	failure := "export deadline exceeded"
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-rt.closed:
			return errors.New("exporter closed")
		default:
		}
		if time.Until(deadline) <= 0 {
			return errors.New(failure)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := rt.client.Do(req)
		var retryAfter string
		if err == nil {
			retryAfter = resp.Header.Get("Retry-After")
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			failure = "collector responded with HTTP " + strconv.Itoa(resp.StatusCode)
			if attempt == maxAttempts || !retryable[resp.StatusCode] {
				return errors.New(failure)
			}
		} else {
			select {
			case <-rt.closed:
				return errors.New("exporter closed")
			default:
			}
			if ctx.Err() != nil {
				return fmt.Errorf("request timed out after %d ms", exportTimeout.Milliseconds())
			}
			failure = transportFailure(err)
			if attempt == maxAttempts {
				return errors.New(failure)
			}
		}
		delay, ok := RetryDelay(retryAfter, time.Now(), deadline, retryBase*time.Duration(attempt))
		if !ok {
			return errors.New(failure)
		}
		select {
		case <-time.After(delay):
		case <-rt.closed:
			return errors.New("exporter closed")
		}
	}
	return errors.New(failure)
}

// RetryDelay is TS retryDelayMilliseconds: Retry-After seconds or an HTTP
// date; invalid or past values use the fallback; a delay that does not fit
// before the deadline suppresses the retry.
func RetryDelay(retryAfter string, now, deadline time.Time, fallback time.Duration) (time.Duration, bool) {
	delay := fallback
	if retryAfter != "" {
		if isDigits(retryAfter) {
			n, err := strconv.ParseFloat(retryAfter, 64)
			if err != nil || n > 1e6 { // far beyond any deadline
				return 0, false
			}
			delay = time.Duration(n * float64(time.Second))
		} else if !(len(retryAfter) > 1 && retryAfter[0] == '-' && isDigits(retryAfter[1:])) {
			if at, err := http.ParseTime(retryAfter); err == nil && !at.Before(now) {
				delay = at.Sub(now)
			}
		}
	}
	if delay < 0 || delay >= deadline.Sub(now) {
		return 0, false
	}
	return delay, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// transportFailure names a connection failure by code, as Bun reports it.
func transportFailure(err error) string {
	var code string
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		code = "ConnectionRefused"
	case errors.Is(err, syscall.ECONNRESET):
		code = "ECONNRESET"
	case errors.As(err, &dnsErr):
		code = "ENOTFOUND"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		code = "EHOSTUNREACH"
	}
	if code == "" {
		return "collector unreachable"
	}
	return "collector unreachable (" + code + ")"
}
