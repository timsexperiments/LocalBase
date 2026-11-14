package system

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// collector records OTLP/HTTP protobuf exports, decoded.
type collector struct {
	mu      sync.Mutex
	traces  []map[string]any
	logs    []map[string]any
	headers []http.Header
	status  int // response status (default 200)
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	switch r.URL.Path {
	case "/base/v1/traces":
		if m, err := otlp.DecodeTraces(b); err == nil {
			c.traces = append(c.traces, m)
		}
	case "/base/v1/logs":
		if m, err := otlp.DecodeLogs(b); err == nil {
			c.logs = append(c.logs, m)
		}
	}
	if c.status != 0 {
		w.WriteHeader(c.status)
	}
}

// spans flattens every exported span with its resource and scope.
func (c *collector) spans() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, req := range c.traces {
		for _, rs := range list(req["resourceSpans"]) {
			rsm := rs.(map[string]any)
			for _, ss := range list(rsm["scopeSpans"]) {
				ssm := ss.(map[string]any)
				for _, s := range list(ssm["spans"]) {
					sm := s.(map[string]any)
					sm["_resource"], sm["_scope"] = rsm["resource"], ssm["scope"]
					out = append(out, sm)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["startTimeUnixNano"].(float64) < out[j]["startTimeUnixNano"].(float64)
	})
	return out
}

func (c *collector) records() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, req := range c.logs {
		for _, rl := range list(req["resourceLogs"]) {
			rlm := rl.(map[string]any)
			for _, sl := range list(rlm["scopeLogs"]) {
				slm := sl.(map[string]any)
				for _, l := range list(slm["logRecords"]) {
					lm := l.(map[string]any)
					lm["_resource"], lm["_scope"] = rlm["resource"], slm["scope"]
					out = append(out, lm)
				}
			}
		}
	}
	return out
}

func list(v any) []any { l, _ := v.([]any); return l }

// otlpRun is one gateway's exports for the scripted requests.
type otlpRun struct {
	col      *collector
	root     string
	requests map[string]string // label → request id
	real     bool
}

const otlpSecret = "otlpHeaderSecret55"

// runOTLP launches a gateway exporting to a local collector, sends the
// scripted requests, and stops it (the final flush).
func runOTLP(t *testing.T, impl string, status int, opts ...harness.Option) *otlpRun {
	t.Helper()
	col := &collector{status: status}
	srv := rangeServer(t, col, false)
	run := runOTLPAt(t, impl, srv.URL+"/base", opts...)
	run.col = col
	return run
}

// runOTLPAt runs the scripted requests against a gateway exporting to
// endpoint; the caller fills in what was received.
func runOTLPAt(t *testing.T, impl, endpoint string, opts ...harness.Option) *otlpRun {
	t.Helper()
	tg, err := harness.Launch(append([]harness.Option{harness.Env([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint, "OTEL_TRACES_SAMPLER=always_on",
		"OTEL_EXPORTER_OTLP_HEADERS=x-auth=" + otlpSecret})}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	run := &otlpRun{root: tg.Root, requests: map[string]string{}, real: tg.RealModel}
	send := func(label, method, path string, headers map[string]string, body string) {
		r := harness.Do(t, method, tg.BaseURL+path, headers, body)
		run.requests[label] = r.Header.Get("X-Localbase-Request-Id")
	}
	send("models", "GET", "/v1/models", harness.Bearer(tg.Key), "")
	send("unmatched", "GET", "/v1/nope/x", harness.Bearer(tg.Key), "")
	send("missing", "GET", "/v1/models", nil, "")
	send("invalid", "GET", "/v1/models", harness.Bearer("lb_not_a_real_key_0123456789abcdef"), "")
	if tg.RealModel {
		send("chat", "POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + tg.Key,
			"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
			`{"model":"qwen2.5-coder-1.5b-instruct-q4_k_m","messages":[{"role":"user","content":"Say hi"}],"max_tokens":4}`)
	}
	tg.Halt() // flushes the exporter; the files stay for the checks
	t.Cleanup(tg.Stop)
	return run
}

// volatileAttr: values that differ per run (ids, durations, pids, ports).
func volatileAttr(k string) bool {
	return strings.HasSuffix(k, "duration_ms") || strings.HasSuffix(k, "request_id") || strings.HasSuffix(k, "principal_id") ||
		k == "localbase.event_id" || k == "pid"
}

func normAttrs(attrs map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range attrs {
		if volatileAttr(k) {
			v = "<volatile>"
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

// tree is a request's spans: the server span and its descendants, with
// structure and stable attributes.
func (r *otlpRun) tree(requestID string) map[string]any {
	spans := r.col.spans()
	var server map[string]any
	for _, s := range spans {
		if a, _ := s["attributes"].(map[string]any); a["localbase.request_id"] == requestID && s["kind"] == 2.0 {
			server = s
		}
	}
	if server == nil {
		return nil
	}
	var describe func(s map[string]any) map[string]any
	describe = func(s map[string]any) map[string]any {
		var children []any
		for _, c := range spans {
			if c["parentSpanId"] == s["spanId"] {
				children = append(children, describe(c))
			}
		}
		sort.Slice(children, func(i, j int) bool {
			return children[i].(map[string]any)["name"].(string) < children[j].(map[string]any)["name"].(string)
		})
		status, _ := s["status"].(map[string]any)
		return map[string]any{"name": s["name"], "kind": s["kind"], "flags": s["flags"], "status": fmt.Sprint(status["code"]),
			"attributes": normAttrs(s["attributes"].(map[string]any)), "children": children,
			"resource": s["_resource"].(map[string]any)["attributes"], "scope": s["_scope"].(map[string]any)["name"]}
	}
	return describe(server)
}

// localEvents reads the gateway's event log by id.
func localEvents(t *testing.T, root string) map[string]map[string]any {
	b, err := os.ReadFile(filepath.Join(root, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, line := range strings.Split(string(b), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["id"] != nil {
			out[e["id"].(string)] = e
		}
	}
	return out
}

// checkRecords: every exported record is a local event, with the TS
// attribute mapping and the event's own trace correlation.
func (r *otlpRun) checkRecords(t *testing.T, impl string) map[string]map[string]any {
	t.Helper()
	events := localEvents(t, r.root)
	byRequest := map[string]map[string]any{} // eventName|request id → record
	for _, rec := range r.col.records() {
		attrs := rec["attributes"].(map[string]any)
		id, _ := attrs["localbase.event_id"].(string)
		e := events[id]
		if e == nil {
			t.Errorf("%s: exported record %v is not in the local log", impl, attrs["event.name"])
			continue
		}
		if attrs["event.name"] != e["eventName"] || attrs["event.category"] != e["category"] || attrs["localbase.component"] != e["component"] ||
			attrs["localbase.runtime"] != e["runtime"] || bodyText(rec["body"]) != e["message"] ||
			rec["severityText"] != strings.ToUpper(e["severity"].(string)) {
			t.Errorf("%s: record %v does not match event %v", impl, attrs, e)
		}
		if ea, ok := e["attributes"].(map[string]any); ok {
			for k, v := range ea {
				if fmt.Sprint(otlpPlain(attrs[k])) != fmt.Sprint(v) {
					t.Errorf("%s: %s attribute %s = %v, event has %v", impl, e["eventName"], k, attrs[k], v)
				}
			}
		}
		if tr, ok := e["trace"].(map[string]any); ok {
			if rec["traceId"] != tr["traceId"] || rec["spanId"] != tr["spanId"] || rec["flags"] != 1.0 {
				t.Errorf("%s: %s record trace %v/%v, event %v", impl, e["eventName"], rec["traceId"], rec["spanId"], tr)
			}
		} else if rec["traceId"] != nil {
			t.Errorf("%s: %s record has a trace the event lacks", impl, e["eventName"])
		}
		if strings.HasPrefix(fmt.Sprint(e["eventName"]), "observability.export") {
			t.Errorf("%s: an exporter diagnostic was exported", impl)
		}
		if rid, ok := e["requestId"].(string); ok {
			byRequest[e["eventName"].(string)+"|"+rid] = rec
		}
	}
	return byRequest
}

func otlpPlain(v any) any {
	if n, ok := v.(otlp.Number); ok {
		return n.Value
	}
	return v
}

// TestDifferentialOTLP: the gateway exports span trees and request log records, correlated to
// their spans, for each request; exporter headers are sent and never logged. The trees and records
// are pinned (they were compared with TS's until step 7).
func TestDifferentialOTLP(t *testing.T) {
	run := runOTLP(t, "go", 0, pinnedFixture(t)...)
	records := run.checkRecords(t, "go")
	run.col.mu.Lock()
	for _, h := range run.col.headers {
		if h.Get("X-Auth") != otlpSecret || h.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("export headers %v", h)
		}
	}
	run.col.mu.Unlock()
	if b, _ := os.ReadFile(filepath.Join(run.root, "logs", "events.jsonl")); strings.Contains(string(b), otlpSecret) {
		t.Errorf("the exporter header reached the local log")
	}
	// A 4xx server span and request record carry the error's type and code, never its message.
	for label, code := range map[string]string{"unmatched": "route_disabled", "missing": "invalid_api_key", "invalid": "invalid_api_key"} {
		span := run.tree(run.requests[label])
		attrs, _ := span["attributes"].(map[string]any)
		if fmt.Sprint(otlpPlain(attrs["localbase.error.code"])) != code || fmt.Sprint(otlpPlain(attrs["localbase.error.type"])) != "invalid_request_error" {
			t.Errorf("%s span error attributes: %v", label, attrs)
		}
		if rec := records["http.request|"+run.requests[label]]; rec == nil || fmt.Sprint(otlpPlain(rec["attributes"].(map[string]any)["error_code"])) != code {
			t.Errorf("%s request record lacks error_code %s: %v", label, code, rec)
		}
	}
	// The version and queue time are left out of the pinned trees, and durations of the records.
	volatile := func(p string) bool {
		return strings.HasSuffix(p, "service.version") || strings.HasSuffix(p, "attributes.localbase.inference.queue.duration_ms")
	}
	for _, label := range []string{"models", "unmatched", "missing", "invalid"} {
		tree := run.tree(run.requests[label])
		if tree == nil {
			t.Errorf("%s: no server span", label)
			continue
		}
		harness.PinJSON(t, label+" span tree", harness.PruneJSON(tree, "", volatile))
		rec := records["http.request|"+run.requests[label]]
		if rec == nil {
			t.Errorf("%s: http.request record missing", label)
			continue
		}
		n := map[string]any{"attributes": normAttrs(rec["attributes"].(map[string]any)), "severity": rec["severityText"], "flags": rec["flags"]}
		harness.PinJSON(t, label+" http.request record", harness.PruneJSON(n, "", func(p string) bool { return strings.HasSuffix(p, "_ms") }))
	}
}

// TestDifferentialOTLPFailures: a failing collector is reported once in the local log, and never
// exported. The reports are pinned (they were compared with TS's until step 7).
func TestDifferentialOTLPFailures(t *testing.T) {
	run := runOTLP(t, "go", 500, pinnedFixture(t)...)
	var got []map[string]any
	for _, e := range localEvents(t, run.root) {
		if strings.HasPrefix(fmt.Sprint(e["eventName"]), "observability.export") {
			a := e["attributes"].(map[string]any)
			got = append(got, map[string]any{"event": e["eventName"], "message": e["message"], "severity": e["severity"],
				"signal": a["signal"], "detail": a["detail"], "component": e["component"], "category": e["category"]})
		}
	}
	sort.Slice(got, func(i, j int) bool { return fmt.Sprint(got[i]) < fmt.Sprint(got[j]) })
	if len(got) == 0 {
		t.Fatal("no export failure was reported")
	}
	// Each signal's first failure, then (at shutdown) a summary when more batches failed: the
	// distinct reports are pinned.
	seen := map[string]bool{}
	var distinct []any
	for _, e := range got {
		if k := fmt.Sprint(e); !seen[k] {
			seen[k] = true
			distinct = append(distinct, e)
		}
	}
	harness.PinJSON(t, "failure reports", distinct)
}

// TestCLIOtelEnvironment: doctor and configure report the effective exporter settings, OTEL_*
// applied (pinned; TS's until step 7), and never print an exporter header.
func TestCLIOtelEnvironment(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	initRoot(t, gr)
	envs := [][]string{
		{"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318/base/", "OTEL_TRACES_SAMPLER=traceidratio", "OTEL_TRACES_SAMPLER_ARG=0.25"},
		{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://traces.example/v1/traces", "OTEL_EXPORTER_OTLP_HEADERS=x-auth=" + otlpSecret},
		{"OTEL_TRACES_SAMPLER=always_off"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL=grpc"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT=http://user:pw@collector.example"},
		{"OTEL_TRACES_SAMPLER_ARG=0.5"},
	}
	for _, env := range envs {
		for _, args := range [][]string{{"--json", "doctor"}, {"--json", "configure", "--defaults", "--no-create-key"}} {
			if r := runCLIEnv(t, "go", gr, env, args...); strings.Contains(r.stdout+r.stderr, otlpSecret) {
				t.Errorf("%v %v: an exporter header was printed", env, args)
			}
		}
	}
}

// bodyText is a record's string body, decoded or as collector JSON.
func bodyText(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m["stringValue"]
	}
	return v
}

// TestOtelShutdownDiagnosticsStayInTheLog (#367): with a collector that
// refuses connections and no console mirroring (as under launchd capture),
// the shutdown flush's failure reports reach events.jsonl, written while
// the file sink is still open, and nothing about OTLP reaches stderr.
func TestOtelShutdownDiagnosticsStayInTheLog(t *testing.T) {
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true,
		env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:" + strconv.Itoa(testutil.FreePort())}}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	e.serve("--no-auth", "--json")
	e.do("GET", "/v1/models", "")
	e.stop()
	logs, _ := filepath.Glob(filepath.Join(tmp, "serve-*.log"))
	for _, p := range logs {
		b, _ := os.ReadFile(p)
		for _, s := range []string{"OTLP", "otel", "observability"} {
			if strings.Contains(string(b), s) {
				t.Errorf("process output mentions %q:\n%s", s, b)
			}
		}
	}
	b, _ := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	var stopped, failed int
	for i, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(line, `"eventName":"gateway.stopped"`):
			stopped = i
		case strings.Contains(line, `"eventName":"observability.export-failed"`):
			failed = i
			if !strings.Contains(line, "collector unreachable (ConnectionRefused)") {
				t.Errorf("failure event: %s", line)
			}
		}
	}
	if failed == 0 || failed < stopped {
		t.Errorf("no shutdown export failure after gateway.stopped in events.jsonl (stopped line %d, failed line %d)", stopped, failed)
	}
}
