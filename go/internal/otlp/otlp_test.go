package otlp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

type golden struct {
	IDs      []string `json:"ids"`
	Sampling []struct {
		Ratio   float64 `json:"ratio"`
		Sampled []bool  `json:"sampled"`
	} `json:"sampling"`
	Now   int64 `json:"now"`
	Retry []struct {
		Value    *string  `json:"value"`
		Fallback int      `json:"fallback"`
		Delay    *float64 `json:"delay"`
	} `json:"retry"`
	Script [][]any           `json:"script"`
	Events []json.RawMessage `json:"events"`
}

func load(t *testing.T) golden {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil || len(g.IDs) < 400 {
		t.Fatalf("golden: %v", err)
	}
	return g
}

// The ratio sampler keeps exactly the traces the JS SDK keeps.
func TestRatioSamplerGolden(t *testing.T) {
	g := load(t)
	for _, s := range g.Sampling {
		for i, id := range g.IDs {
			var tid [16]byte
			b, _ := hex.DecodeString(id)
			copy(tid[:], b)
			if got := ratioSample(s.Ratio, tid); got != s.Sampled[i] {
				t.Errorf("ratio %v id %s: %v, want %v", s.Ratio, id, got, s.Sampled[i])
			}
		}
	}
}

func TestRetryDelayGolden(t *testing.T) {
	g := load(t)
	now := time.UnixMilli(g.Now)
	deadline := now.Add(5 * time.Second)
	for _, c := range g.Retry {
		v := ""
		if c.Value != nil {
			v = *c.Value
		}
		d, ok := RetryDelay(v, now, deadline, time.Duration(c.Fallback)*time.Millisecond)
		switch {
		case c.Delay == nil && ok:
			t.Errorf("%q: delay %v, want none", v, d)
		case c.Delay != nil && (!ok || d.Milliseconds() != int64(*c.Delay)):
			t.Errorf("%q: delay %v %v, want %v ms", v, d, ok, *c.Delay)
		}
	}
}

// The reporter emits the same events, at the same times, with the same
// attributes as TS over a scripted outage.
func TestReporterGolden(t *testing.T) {
	g := load(t)
	var clock time.Time
	base := time.UnixMilli(0)
	var got []map[string]any
	r := NewReporter(func(severity, message string, attrs map[string]any) {
		got = append(got, map[string]any{"at": float64(clock.Sub(base).Milliseconds()), "severity": severity, "message": message,
			"attributes": jsjson.FromGo(attrs)})
	}, func() time.Time { return clock })
	r.Interval = time.Second
	for _, step := range g.Script {
		clock = base.Add(time.Duration(step[0].(float64)) * time.Millisecond)
		parts := strings.SplitN(step[1].(string), ":", 3)
		switch parts[0] {
		case "fail":
			r.Failure(parts[1], Report{Signal: parts[1], Severity: "warn", Message: "OTLP export failed.", Detail: parts[2], Items: 3,
				Attrs: map[string]any{"batchSize": 3}})
		case "recover":
			r.Recovered(parts[1])
		default:
			r.Flush()
		}
	}
	var want []any
	for _, e := range g.Events {
		var v any
		json.Unmarshal(e, &v)
		want = append(want, v)
	}
	var have []any
	for _, e := range got {
		b, _ := json.Marshal(e)
		var v any
		json.Unmarshal(b, &v)
		have = append(have, v)
	}
	if !reflect.DeepEqual(have, want) {
		gb, _ := json.MarshalIndent(have, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("events:\n%s\nwant\n%s", gb, wb)
	}
}

// End to end against a local collector: spans and log records decode with
// the structure the JS exporter writes, a retryable status is retried, a
// failing collector is reported once and its recovery reported.
func TestExportToCollector(t *testing.T) {
	var mu sync.Mutex
	var traces, logs []map[string]any
	statuses := []int{503, 200}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("X-Token") != "abc" {
			t.Errorf("headers: %v", r.Header)
		}
		status := 200
		if calls < len(statuses) && strings.HasSuffix(r.URL.Path, "/traces") {
			status = statuses[calls]
			calls++
		}
		if status == 200 {
			if strings.HasSuffix(r.URL.Path, "/traces") {
				m, err := DecodeTraces(b)
				if err != nil {
					t.Error(err)
				}
				traces = append(traces, m)
			} else {
				m, err := DecodeLogs(b)
				if err != nil {
					t.Error(err)
				}
				logs = append(logs, m)
			}
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	cfg, err := obs.ResolveOtel(srv.URL, "x-token=abc", 100, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var diags []string
	rt := New(cfg, "test", func(sev, msg string, attrs map[string]any) {
		diags = append(diags, sev+" "+msg+" "+fmt.Sprint(attrs["detail"]))
	})
	h := http.Header{}
	h.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	ctx, server := rt.Start(Extract(context.Background(), h), "GET /v1/models", KindServer, "http.route", "/v1/models")
	server.SetAttr("http.response.status_code", 200)
	server.SetAttr("localbase.http.response.duration_ms", 1.5)
	_, child := rt.Start(ctx, "localbase.backend.start", KindInternal, "localbase.backend", "llama-server")
	child.RecordException("Error", "boom")
	child.SetError("boom")
	child.End()
	out := http.Header{}
	Inject(ctx, out)
	if !strings.HasPrefix(out.Get("Traceparent"), "00-0af7651916cd43dd8448eb211c80319c-"+server.Context().SpanIDHex()) {
		t.Errorf("traceparent %q", out.Get("Traceparent"))
	}
	ev, _ := jsjson.Parse([]byte(`{"schemaVersion":2,"id":"0b7c4a62-0f3b-4f6f-9d0e-6a8a1d2c3e4f","timestamp":"2026-10-20T10:00:00.123Z",` +
		`"severity":"warn","eventName":"http.request","category":"http","component":"gateway","runtime":"gateway","message":"GET /v1/models -> 200",` +
		`"requestId":"lbreq_1","http":{"method":"GET","path":"/v1/models","status":200,"durationMs":1.5},"attributes":{"auth_outcome":"missing"},` +
		`"trace":{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"` + server.Context().SpanIDHex() + `"}}`))
	rt.Emit(ev.(*jsjson.Object))
	server.End()
	rt.Shutdown()
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 || len(traces) != 1 || len(logs) != 1 {
		t.Fatalf("calls %d traces %d logs %d", calls, len(traces), len(logs))
	}
	spans := traces[0]["resourceSpans"].([]any)[0].(map[string]any)
	res := spans["resource"].(map[string]any)["attributes"].(map[string]any)
	if res["service.name"] != "local-base" || res["service.namespace"] != "localbase" || res["service.version"] != "test" {
		t.Errorf("resource %v", res)
	}
	list := spans["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(list) != 2 {
		t.Fatalf("spans %v", list)
	}
	c, s := list[0].(map[string]any), list[1].(map[string]any)
	if c["name"] != "localbase.backend.start" || c["kind"] != 1.0 || c["parentSpanId"] != s["spanId"] || c["flags"] != 257.0 ||
		c["status"].(map[string]any)["code"] != 2.0 || c["status"].(map[string]any)["message"] != "boom" {
		t.Errorf("child %v", c)
	}
	if e := c["events"].([]any)[0].(map[string]any); e["name"] != "exception" ||
		e["attributes"].(map[string]any)["exception.message"] != "boom" {
		t.Errorf("exception event %v", e)
	}
	if s["kind"] != 2.0 || s["parentSpanId"] != "b7ad6b7169203331" || s["flags"] != 769.0 || s["traceId"] != "0af7651916cd43dd8448eb211c80319c" ||
		s["attributes"].(map[string]any)["http.response.status_code"] != (Number{200, true}) ||
		s["attributes"].(map[string]any)["localbase.http.response.duration_ms"] != (Number{1.5, false}) {
		t.Errorf("server %v", s)
	}
	rec := logs[0]["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)
	attrs := rec["attributes"].(map[string]any)
	if rec["severityNumber"] != 13.0 || rec["severityText"] != "WARN" || rec["flags"] != 1.0 || rec["traceId"] != "0af7651916cd43dd8448eb211c80319c" ||
		attrs["http.route"] != "/v1/models" || attrs["localbase.request_id"] != "lbreq_1" || attrs["auth_outcome"] != "missing" ||
		attrs["localbase.schema_version"] != (Number{2, true}) || rec["timeUnixNano"] != float64(time.Date(2026, 10, 20, 10, 0, 0, 123e6, time.UTC).UnixNano()) {
		t.Errorf("log record %v", rec)
	}
	// One retry on 503 and no failure left to report.
	if len(diags) != 0 {
		t.Errorf("diagnostics %v", diags)
	}
}

func TestExportFailureReporting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer srv.Close()
	cfg, _ := obs.ResolveOtel(srv.URL, "", 100, func(string) string { return "" })
	var mu sync.Mutex
	var diags []string
	rt := New(cfg, "test", func(sev, msg string, attrs map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		diags = append(diags, fmt.Sprintf("%s|%s|%v|%v", sev, msg, attrs["signal"], attrs["detail"]))
	})
	for i := 0; i < 3; i++ {
		_, s := rt.Start(context.Background(), "x", KindInternal)
		s.End()
		rt.spans.flushOne()
	}
	rt.Shutdown()
	want := []string{"warn|OTLP export failed.|traces|collector responded with HTTP 400", "warn|OTLP export still failing.|traces|collector responded with HTTP 400"}
	if !reflect.DeepEqual(diags, want) {
		t.Errorf("diagnostics %v", diags)
	}
	// Unreachable: the transport code is named.
	cfg2, _ := obs.ResolveOtel("http://127.0.0.1:1", "", 100, func(string) string { return "" })
	rt2 := New(cfg2, "test", nil)
	if err := rt2.send(cfg2.TracesEndpoint, nil, []byte{}, "spans", 1); err == nil || err.Error() != "collector unreachable (ConnectionRefused)" {
		t.Errorf("unreachable: %v", err)
	}
	rt2.Shutdown()
}

// Without a sampled parent, traceidratio 0 records nothing but still
// propagates a context; a nil runtime is inert.
func TestSamplingAndDisabled(t *testing.T) {
	cfg, _ := obs.ResolveOtel("http://127.0.0.1:1", "", 0, func(string) string { return "" })
	rt := New(cfg, "test", nil)
	defer rt.Shutdown()
	ctx, s := rt.Start(context.Background(), "x", KindServer)
	if s.Recording() || !s.Context().Valid() {
		t.Fatalf("unsampled span: %+v", s.Context())
	}
	h := http.Header{}
	Inject(ctx, h)
	if !strings.HasSuffix(h.Get("Traceparent"), "-00") {
		t.Errorf("traceparent %q", h.Get("Traceparent"))
	}
	// A sampled remote parent is followed (parent-based).
	in := http.Header{}
	in.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	if _, s := rt.Start(Extract(context.Background(), in), "y", KindServer); !s.Recording() {
		t.Error("a sampled remote parent was not followed")
	}
	var off *Runtime
	ctx, span := off.Start(context.Background(), "z", KindServer)
	span.SetAttr("a", 1)
	span.End()
	if span != nil || FromContext(ctx) != nil || off.Enabled() {
		t.Error("a disabled runtime traced")
	}
	off.Emit(nil)
	off.Shutdown()
}

// A collector that never answers cannot hold shutdown past the close
// deadline, and the lost batch is reported.
func TestShutdownWithHungCollector(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg, _ := obs.ResolveOtel(srv.URL, "", 100, func(string) string { return "" })
	var mu sync.Mutex
	var diags []string
	rt := New(cfg, "test", func(sev, msg string, attrs map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		diags = append(diags, msg+" "+fmt.Sprint(attrs["detail"]))
	})
	_, s := rt.Start(context.Background(), "x", KindServer)
	s.End()
	start := time.Now()
	rt.Shutdown()
	if d := time.Since(start); d > closeDeadline+2*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(diags, "\n")
	if !strings.Contains(joined, "OTLP export failed.") ||
		!(strings.Contains(joined, "request timed out after 5000 ms") || strings.Contains(joined, "exporter closed")) {
		t.Errorf("diagnostics: %v", diags)
	}
}

// A collector that accepts a batch but rejects part of it (partial success,
// protobuf or JSON) feeds the throttled failure reporter; a full success
// afterwards reports recovery.
func TestPartialSuccessIsReported(t *testing.T) {
	for _, format := range []string{"protobuf", "json"} {
		t.Run(format, func(t *testing.T) {
			var mu sync.Mutex
			reject := true
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				if !reject {
					return
				}
				if format == "json" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"span too large; token=abcdef123456"}}`)
					return
				}
				var inner, outer buf
				inner.uint(1, 2)
				inner.str(2, "span too large")
				outer.message(1, inner)
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Write(outer)
			}))
			defer srv.Close()
			cfg, _ := obs.ResolveOtel(srv.URL, "", 100, func(string) string { return "" })
			var dmu sync.Mutex
			var diags []string
			rt := New(cfg, "test", func(sev, msg string, attrs map[string]any) {
				dmu.Lock()
				defer dmu.Unlock()
				diags = append(diags, sev+"|"+msg+"|"+fmt.Sprint(attrs["detail"]))
			})
			for i := 0; i < 2; i++ {
				for j := 0; j < 3; j++ {
					_, s := rt.Start(context.Background(), "x", KindInternal)
					s.End()
				}
				rt.spans.flushOne()
			}
			mu.Lock()
			reject = false
			mu.Unlock()
			_, s := rt.Start(context.Background(), "x", KindInternal)
			s.End()
			rt.spans.flushOne()
			rt.Shutdown()
			detail := "collector rejected 2 of 3 spans: span too large"
			if format == "json" {
				detail += "; token=[REDACTED]"
			}
			want := []string{"warn|OTLP export failed.|" + detail, "info|OTLP export recovered.|" + detail}
			dmu.Lock()
			defer dmu.Unlock()
			if !reflect.DeepEqual(diags, want) {
				t.Errorf("diagnostics:\n %q\nwant\n %q", diags, want)
			}
		})
	}
}
