package otlp

import (
	"encoding/hex"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// anyValue encodes an attribute value as the JS transformer does: strings,
// booleans, integral numbers as int_value, other numbers as double_value.
func anyValue(v any) buf {
	var b buf
	switch x := v.(type) {
	case string:
		b.tag(1, 2)
		b.varint(uint64(len(x)))
		b = append(b, x...)
	case bool:
		b.tag(2, 0)
		if x {
			b.varint(1)
		} else {
			b.varint(0)
		}
	case int:
		b.tag(3, 0)
		b.varint(uint64(int64(x)))
	case int64:
		b.tag(3, 0)
		b.varint(uint64(x))
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) && math.Abs(x) <= 1<<53 {
			b.tag(3, 0)
			b.varint(uint64(int64(x)))
		} else {
			b.double(4, x)
		}
	}
	return b
}

func encodeKVs(field int, attrs []attr) buf {
	var out buf
	for _, a := range attrs {
		var kv buf
		kv.str(1, a.key)
		kv.message(2, anyValue(a.value))
		out.message(field, kv)
	}
	return out
}

func nanos(t time.Time) uint64 { return uint64(t.UnixNano()) }

// Span flags: W3C trace flags plus the has-is-remote bit, and is-remote
// for a remote parent (JS buildSpanFlagsFrom).
func spanFlags(s *Span) uint32 {
	f := uint32(0x100)
	if s.ctx.Sampled {
		f |= 1
	}
	if s.parent.Remote {
		f |= 0x200
	}
	return f
}

func (rt *Runtime) envelope(field int, records buf) []byte {
	var scoped buf
	scoped.message(1, rt.scope)
	scoped = append(scoped, records...)
	var resource buf
	resource.message(1, rt.resource)
	resource.message(2, scoped)
	var req buf
	req.message(field, resource)
	return req
}

// encodeSpans is an ExportTraceServiceRequest.
func (rt *Runtime) encodeSpans(spans []*Span) []byte {
	var records buf
	for _, s := range spans {
		s.mu.Lock()
		var m buf
		m.bytes(1, s.ctx.TraceID[:])
		m.bytes(2, s.ctx.SpanID[:])
		m.str(3, s.ctx.TraceState)
		if s.parent.Valid() {
			m.bytes(4, s.parent.SpanID[:])
		}
		m.str(5, s.name)
		m.uint(6, uint64(s.kind))
		m.fixed64(7, nanos(s.start))
		m.fixed64(8, nanos(s.end))
		m = append(m, encodeKVs(9, s.attrs)...)
		m.uintAlways(10, 0)
		for _, e := range s.events {
			var ev buf
			ev.fixed64(1, nanos(e.time))
			ev.str(2, e.name)
			ev = append(ev, encodeKVs(3, e.attrs)...)
			m.message(11, ev)
		}
		m.uintAlways(12, 0)
		m.uintAlways(14, 0)
		var st buf
		st.str(2, s.statusMsg)
		st.uintAlways(3, uint64(s.status))
		m.message(15, st)
		m.fixed32(16, spanFlags(s))
		s.mu.Unlock()
		records.message(2, m)
	}
	return rt.envelope(1, records)
}

// logRecord is one exported log event.
type logRecord struct {
	time, observed time.Time
	severity       int
	severityText   string
	body           string
	attrs          []attr
	traceID        []byte
	spanID         []byte
}

var severityNumbers = map[string]int{"debug": 5, "info": 9, "warn": 13, "error": 17}

// Emit exports a stored log event (TS ActiveOtelRuntime.emit): the message
// is the body, the event fields and attributes become attributes, and its
// trace correlation becomes the record's context.
func (rt *Runtime) Emit(event *jsjson.Object) {
	if rt == nil || rt.logs == nil || event == nil {
		return
	}
	get := func(o *jsjson.Object, k string) any { v, _ := o.Get(k); return v }
	str := func(o *jsjson.Object, k string) string { s, _ := get(o, k).(string); return s }
	r := logRecord{observed: time.Now(), body: str(event, "message")}
	if t, err := time.Parse(time.RFC3339Nano, str(event, "timestamp")); err == nil {
		r.time = t.Truncate(time.Millisecond)
	}
	sev := str(event, "severity")
	r.severity, r.severityText = severityNumbers[sev], upper(sev)
	r.attrs = []attr{{"localbase.event_id", str(event, "id")}, {"localbase.schema_version", get(event, "schemaVersion")},
		{"event.name", str(event, "eventName")}, {"event.category", str(event, "category")},
		{"localbase.component", str(event, "component")}, {"localbase.runtime", str(event, "runtime")}}
	if id := str(event, "requestId"); id != "" {
		r.attrs = append(r.attrs, attr{"localbase.request_id", id})
	}
	if h, ok := get(event, "http").(*jsjson.Object); ok {
		r.attrs = append(r.attrs, attr{"http.request.method", get(h, "method")}, attr{"http.route", get(h, "path")},
			attr{"http.response.status_code", get(h, "status")}, attr{"localbase.http.duration_ms", get(h, "durationMs")})
	}
	if e, ok := get(event, "error").(*jsjson.Object); ok {
		r.attrs = append(r.attrs, attr{"error.type", get(e, "type")}, attr{"error.message", get(e, "message")})
		if c := str(e, "code"); c != "" {
			r.attrs = append(r.attrs, attr{"error.code", c})
		}
	}
	if a, ok := get(event, "attributes").(*jsjson.Object); ok {
		for _, k := range a.Keys() {
			r.attrs = setAttr(r.attrs, k, get(a, k))
		}
	}
	if t, ok := get(event, "trace").(*jsjson.Object); ok {
		r.traceID, _ = hex.DecodeString(str(t, "traceId"))
		r.spanID, _ = hex.DecodeString(str(t, "spanId"))
	}
	rt.logs.add(r)
}

func setAttr(attrs []attr, k string, v any) []attr {
	for i := range attrs {
		if attrs[i].key == k {
			attrs[i].value = v
			return attrs
		}
	}
	return append(attrs, attr{k, v})
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// encodeLogs is an ExportLogsServiceRequest.
func (rt *Runtime) encodeLogs(records []logRecord) []byte {
	var out buf
	for _, r := range records {
		var m buf
		m.fixed64(1, nanos(r.time))
		m.uint(2, uint64(r.severity))
		m.str(3, r.severityText)
		var body buf
		body.message(5, anyValue(r.body))
		m = append(m, body...)
		m = append(m, encodeKVs(6, r.attrs)...)
		m.uintAlways(7, 0)
		if len(r.traceID) == 16 && len(r.spanID) == 8 {
			m.fixed32(8, 1)
			m.bytes(9, r.traceID)
			m.bytes(10, r.spanID)
		}
		m.fixed64(11, nanos(r.observed))
		out.message(2, m)
	}
	return rt.envelope(1, out)
}

// --- export failures -------------------------------------------------------------

// SummaryInterval bounds how often one failing channel logs during an outage.
const SummaryInterval = 10 * time.Minute

// Report is one failed export (TS ExportFailureReport).
type Report struct {
	Signal, Severity, Message, Summary, Detail string
	Items                                      int
	Attrs                                      map[string]any
}

type outage struct {
	signal, summary                    string
	since, lastFailure, lastReport     time.Time
	detail, latest                     string
	failures, suppressed, suppressedIt int
}

// Reporter collapses repeated failures into one event per outage (TS
// ExportFailureReporter, #358): the first failure is reported in full,
// repeats are counted and summarized at most once per interval, recovery
// is reported, and Flush reports what is still uncovered.
type Reporter struct {
	mu       sync.Mutex
	outages  map[string]*outage
	diag     Diagnostic
	now      func() time.Time
	Interval time.Duration
}

func NewReporter(diag Diagnostic, now func() time.Time) *Reporter {
	return &Reporter{outages: map[string]*outage{}, diag: diag, now: now, Interval: SummaryInterval}
}

// Failing reports whether a channel is in an outage.
func (r *Reporter) Failing(channel string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outages[channel] != nil
}

func (r *Reporter) Failure(channel string, rep Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	o := r.outages[channel]
	if o != nil && now.Sub(o.lastFailure) >= r.Interval {
		if o.suppressed > 0 {
			r.summarize(o, o.lastFailure)
		}
		delete(r.outages, channel)
		o = nil
	}
	if o == nil {
		summary := rep.Summary
		if summary == "" {
			summary = "OTLP export still failing."
		}
		r.outages[channel] = &outage{signal: rep.Signal, summary: summary, since: now, lastFailure: now, lastReport: now,
			detail: rep.Detail, latest: rep.Detail, failures: 1}
		attrs := map[string]any{"signal": rep.Signal}
		for k, v := range rep.Attrs {
			attrs[k] = v
		}
		attrs["detail"] = rep.Detail
		r.diag(rep.Severity, rep.Message, attrs)
		return
	}
	o.lastFailure = now
	o.failures++
	o.suppressed++
	o.suppressedIt += rep.Items
	o.latest = rep.Detail
	if now.Sub(o.lastReport) >= r.Interval {
		r.summarize(o, now)
	}
}

func (r *Reporter) Recovered(channel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.outages[channel]
	if o == nil {
		return
	}
	delete(r.outages, channel)
	r.diag("info", "OTLP export recovered.", r.attributes(o, r.now()))
}

func (r *Reporter) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	keys := make([]string, 0, len(r.outages))
	for k := range r.outages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if o := r.outages[k]; o.suppressed > 0 {
			r.summarize(o, now)
		}
	}
}

func (r *Reporter) summarize(o *outage, now time.Time) {
	r.diag("warn", o.summary, r.attributes(o, now))
	o.lastReport, o.suppressed, o.suppressedIt = now, 0, 0
}

func (r *Reporter) attributes(o *outage, now time.Time) map[string]any {
	a := map[string]any{"signal": o.signal, "failingForMs": now.Sub(o.since).Milliseconds(), "failures": o.failures,
		"suppressedFailures": o.suppressed, "detail": o.detail}
	if o.suppressedIt > 0 {
		a["suppressedItems"] = o.suppressedIt
	}
	if o.latest != o.detail {
		a["latestDetail"] = o.latest
	}
	return a
}
