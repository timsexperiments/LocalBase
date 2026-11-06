package otlp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Span kinds (OTLP values).
const (
	KindInternal = 1
	KindServer   = 2
	KindClient   = 3
)

// Status codes (OTLP values).
const (
	StatusUnset = 0
	StatusError = 2
)

// SpanContext identifies a span across processes (W3C trace context).
type SpanContext struct {
	TraceID    [16]byte
	SpanID     [8]byte
	Sampled    bool
	Remote     bool
	TraceState string
}

func (c SpanContext) Valid() bool { return c.TraceID != [16]byte{} && c.SpanID != [8]byte{} }

// TraceIDHex and SpanIDHex are the log correlation forms.
func (c SpanContext) TraceIDHex() string { return hex.EncodeToString(c.TraceID[:]) }
func (c SpanContext) SpanIDHex() string  { return hex.EncodeToString(c.SpanID[:]) }

type attr struct {
	key   string
	value any
}

type event struct {
	time  time.Time
	name  string
	attrs []attr
}

// Span is one operation. A span that was not sampled records nothing but
// still carries its context, so propagation keeps the trace intact. Every
// method is safe on a nil span.
type Span struct {
	rt     *Runtime
	ctx    SpanContext
	parent SpanContext
	name   string
	kind   int
	start  time.Time

	mu        sync.Mutex
	end       time.Time
	attrs     []attr
	events    []event
	status    int
	statusMsg string
	ended     bool
}

// Context returns the span's identity.
func (s *Span) Context() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.ctx
}

// Recording reports whether the span will be exported.
func (s *Span) Recording() bool { return s != nil && s.rt != nil && s.ctx.Sampled }

// SetAttr sets an attribute (string, bool, int kinds or float64); an
// existing key keeps its position, as JS objects do.
func (s *Span) SetAttr(key string, value any) {
	if !s.Recording() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for i := range s.attrs {
		if s.attrs[i].key == key {
			s.attrs[i].value = value
			return
		}
	}
	if len(s.attrs) < 128 {
		s.attrs = append(s.attrs, attr{key, value})
	}
}

// SetError marks the span failed.
func (s *Span) SetError(message string) {
	if !s.Recording() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.status, s.statusMsg = StatusError, message
	}
}

// RecordException adds the exception event TS records for a thrown error,
// already redacted by the caller.
func (s *Span) RecordException(typ, message string) {
	if !s.Recording() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.events = append(s.events, event{time.Now(), "exception",
			[]attr{{"exception.type", typ}, {"exception.message", message}}})
	}
}

// End finishes the span and queues it for export.
func (s *Span) End() {
	if !s.Recording() {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.end = true, time.Now()
	s.mu.Unlock()
	s.rt.spans.add(s)
}

// --- context -------------------------------------------------------------------

type spanKey struct{}

// WithSpan returns ctx carrying span as the active span.
func WithSpan(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// FromContext returns the active span, or nil.
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// Correlation returns the trace and span ids of the active span when it is
// recorded (TS spanCorrelation).
func Correlation(ctx context.Context) (traceID, spanID string, ok bool) {
	s := FromContext(ctx)
	if !s.Recording() || !s.ctx.Valid() {
		return "", "", false
	}
	return s.ctx.TraceIDHex(), s.ctx.SpanIDHex(), true
}

// --- W3C trace context ----------------------------------------------------------

var traceparentRe = regexp.MustCompile(`^\s?([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-.*)?\s?$`)

// Extract reads a remote parent from traceparent/tracestate (TS
// W3CTraceContextPropagator).
func Extract(ctx context.Context, h http.Header) context.Context {
	m := traceparentRe.FindStringSubmatch(h.Get("Traceparent"))
	if m == nil || m[1] == "ff" || (m[1] == "00" && m[5] != "") {
		return ctx
	}
	var sc SpanContext
	t, _ := hex.DecodeString(m[2])
	sp, _ := hex.DecodeString(m[3])
	fl, _ := hex.DecodeString(m[4])
	copy(sc.TraceID[:], t)
	copy(sc.SpanID[:], sp)
	if !sc.Valid() {
		return ctx
	}
	sc.Sampled, sc.Remote = fl[0]&1 == 1, true
	sc.TraceState = strings.Join(h.Values("Tracestate"), ",")
	return WithSpan(ctx, &Span{ctx: sc})
}

// Inject writes the active span's context for a downstream request.
func Inject(ctx context.Context, h http.Header) {
	s := FromContext(ctx)
	if s == nil || !s.ctx.Valid() {
		return
	}
	flags := "00"
	if s.ctx.Sampled {
		flags = "01"
	}
	h.Set("Traceparent", "00-"+s.ctx.TraceIDHex()+"-"+s.ctx.SpanIDHex()+"-"+flags)
	if s.ctx.TraceState != "" {
		h.Set("Tracestate", s.ctx.TraceState)
	}
}

// --- sampling -------------------------------------------------------------------

// ratioSample is the JS TraceIdRatioBasedSampler: the trace id's four
// 32-bit words XORed, below floor(ratio × 0xffffffff).
func ratioSample(ratio float64, id [16]byte) bool {
	if math.IsNaN(ratio) || ratio <= 0 {
		return false
	}
	bound := uint64(math.Floor(ratio * 0xffffffff))
	if ratio >= 1 {
		bound = 0x100000000
	}
	var acc uint32
	for i := 0; i < 16; i += 4 {
		acc ^= uint32(id[i])<<24 | uint32(id[i+1])<<16 | uint32(id[i+2])<<8 | uint32(id[i+3])
	}
	return uint64(acc) < bound
}

func (rt *Runtime) sample(parent SpanContext, id [16]byte) bool {
	root := func() bool {
		switch rt.cfg.Sampler {
		case "always_on", "parentbased_always_on":
			return true
		case "always_off", "parentbased_always_off":
			return false
		}
		return ratioSample(rt.cfg.SampleRatio, id)
	}
	switch rt.cfg.Sampler {
	case "always_on", "always_off", "traceidratio":
		return root()
	}
	if parent.Valid() {
		return parent.Sampled
	}
	return root()
}

func randomID(b []byte) {
	for {
		rand.Read(b)
		for _, x := range b {
			if x != 0 {
				return
			}
		}
	}
}

// Start begins a span under ctx's active span (or remote parent) and
// returns ctx with the new span active. A nil runtime returns a nil span.
func (rt *Runtime) Start(ctx context.Context, name string, kind int, attrs ...any) (context.Context, *Span) {
	if rt == nil {
		return ctx, nil
	}
	parent := FromContext(ctx).Context()
	s := &Span{rt: rt, name: name, kind: kind, start: time.Now()}
	if parent.Valid() {
		s.parent = parent
		s.ctx.TraceID, s.ctx.TraceState = parent.TraceID, parent.TraceState
	} else {
		randomID(s.ctx.TraceID[:])
	}
	randomID(s.ctx.SpanID[:])
	s.ctx.Sampled = rt.sample(parent, s.ctx.TraceID)
	for i := 0; i+1 < len(attrs); i += 2 {
		s.SetAttr(attrs[i].(string), attrs[i+1])
	}
	return WithSpan(ctx, s), s
}
