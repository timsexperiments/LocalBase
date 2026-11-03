package usage

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var noon = time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)

func newRecorder(t *testing.T, logs *bytes.Buffer) (*Recorder, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	if logs != nil {
		log = slog.New(slog.NewJSONHandler(logs, nil))
	}
	r, err := New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return noon }
	return r, st
}

func event(status int, d time.Duration) Event {
	return Event{Kind: "api-key", Principal: "key_a", Route: "chat", Model: "m", Status: status, Duration: d, At: noon}
}

func report(t *testing.T, r *Recorder, q Query) *Report {
	t.Helper()
	if q.To.IsZero() {
		q.To = noon.Add(time.Hour)
	}
	if q.From.IsZero() {
		q.From = noon.Add(-24 * time.Hour)
	}
	got, err := r.Report(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFlushWritesAndMerges(t *testing.T) {
	r, _ := newRecorder(t, nil)
	ctx := context.Background()
	e := event(200, 100*time.Millisecond)
	e.InputTokens, e.OutputTokens, e.HasTokens = 10, 5, true
	r.Add(e)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	r.Add(e)
	r.Add(event(200, 200*time.Millisecond)) // no tokens reported
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got := report(t, r, Query{}).Totals
	if got.Requests != 3 || got.InputTokens != 20 || got.OutputTokens != 10 || got.RequestsWithTokens != 2 {
		t.Errorf("totals: %+v", got)
	}
	if got.Latency.P50 == nil || !got.Latency.Approximate {
		t.Errorf("latency: %+v", got.Latency)
	}
}

// What is not yet written still counts in an answer from the running gateway.
func TestReportIncludesPending(t *testing.T) {
	r, st := newRecorder(t, nil)
	r.Add(event(200, time.Millisecond))
	if got := report(t, r, Query{}).Totals.Requests; got != 1 {
		t.Errorf("before a flush: %d", got)
	}
	var rows int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("rows %d, %v", rows, err)
	}
	// Written and pending add up, without counting anything twice.
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Add(event(200, time.Millisecond))
	if got := report(t, r, Query{}).Totals.Requests; got != 2 {
		t.Errorf("after a flush and one more: %d", got)
	}
}

// One row per hour, not per request: no timeline can be rebuilt from it.
func TestHourlyRowsOnly(t *testing.T) {
	r, st := newRecorder(t, nil)
	for i := range 50 {
		e := event(200, time.Millisecond)
		e.At = noon.Add(time.Duration(i) * time.Second)
		r.Add(e)
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows int
	var hour int64
	if err := st.DB.QueryRow(`SELECT COUNT(*), MIN(hour) FROM usage_hourly`).Scan(&rows, &hour); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || hour != noon.Truncate(time.Hour).Unix() {
		t.Errorf("%d rows, hour %d", rows, hour)
	}
	var cols []string
	list, err := st.DB.Query(`SELECT name FROM pragma_table_info('usage_hourly')`)
	if err != nil {
		t.Fatal(err)
	}
	defer list.Close()
	for list.Next() {
		var c string
		list.Scan(&c)
		cols = append(cols, c)
	}
	// A new column is a decision about what is stored (DESIGN §32.1): make it on purpose.
	want := []string{"hour", "principal_kind", "principal_id", "route", "model_id", "status", "requests", "with_tokens",
		"input_tokens", "output_tokens", "duration_ms", "queue_ms", "latency"}
	if !slices.Equal(cols, want) {
		t.Errorf("columns %v, want %v", cols, want)
	}
}

// A route outside the fixed list is dropped, so a label cannot become free text.
func TestUnknownRouteIsDropped(t *testing.T) {
	r, _ := newRecorder(t, nil)
	e := event(200, time.Millisecond)
	e.Route = "free text from somewhere"
	r.Add(e)
	if got := report(t, r, Query{}).Totals.Requests; got != 0 {
		t.Errorf("counted: %d", got)
	}
}

func TestFlushFailureKeepsCounts(t *testing.T) {
	var logs bytes.Buffer
	r, st := newRecorder(t, &logs)
	r.Add(event(200, time.Millisecond))
	good := r.st
	bad, err := store.Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	bad.Close()
	r.st = bad
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("a flush to a closed database should fail")
	}
	if !strings.Contains(logs.String(), "usage.flush-failed") {
		t.Errorf("logs: %s", logs.String())
	}
	r.st = good
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rows int
	st.DB.QueryRow(`SELECT COALESCE(SUM(requests),0) FROM usage_hourly`).Scan(&rows)
	if rows != 1 {
		t.Errorf("after the retry: %d requests, want the 1 kept", rows)
	}
}

func TestOutcomes(t *testing.T) {
	r, _ := newRecorder(t, nil)
	// 408 is the gateway's own body deadline (DESIGN §31.1): an error. 499 is the client going away: not one.
	for _, s := range []int{200, 200, 202, 400, 403, 404, 408, 422, 429, 499, 500, 502, 503} {
		r.Add(event(s, time.Millisecond))
	}
	got := report(t, r, Query{}).Totals
	if got.Requests != 13 || got.Refused != 6 || got.Failed != 3 || got.Cancelled != 1 {
		t.Errorf("%+v", got)
	}
	if want := 9.0 / 13.0; math.Abs(got.ErrorRate-want) > 1e-9 {
		t.Errorf("error rate %v, want %v: a client going away is not an error", got.ErrorRate, want)
	}
}

func TestRecordingOff(t *testing.T) {
	r, st := newRecorder(t, nil)
	ctx := context.Background()
	r.Add(event(200, time.Millisecond))
	r.Flush(ctx)
	if err := r.SetRecording(ctx, false); err != nil {
		t.Fatal(err)
	}
	r.Add(event(200, time.Millisecond))
	if got := report(t, r, Query{}); got.Totals.Requests != 1 || got.Recording {
		t.Errorf("off: %d requests, recording %v; what exists stays", got.Totals.Requests, got.Recording)
	}
	// It stays off across a restart, and comes back on when asked.
	again, err := New(st, r.log)
	if err != nil {
		t.Fatal(err)
	}
	again.now = r.now
	again.Add(event(200, time.Millisecond))
	if got := report(t, again, Query{}).Totals.Requests; got != 1 {
		t.Errorf("after a restart: %d", got)
	}
	if err := again.SetRecording(ctx, true); err != nil {
		t.Fatal(err)
	}
	again.Add(event(200, time.Millisecond))
	if got := report(t, again, Query{}).Totals.Requests; got != 2 {
		t.Errorf("back on: %d", got)
	}
}

func TestRecordingOnByDefault(t *testing.T) {
	r, _ := newRecorder(t, nil)
	if got := report(t, r, Query{}); !got.Recording || got.RetentionDays != DefaultRetentionDays || DefaultRetentionDays != 30 {
		t.Errorf("recording %v, retention %d", got.Recording, got.RetentionDays)
	}
}

func TestRetention(t *testing.T) {
	r, st := newRecorder(t, nil)
	ctx := context.Background()
	for _, bad := range []int{0, -1, 366} {
		if err := r.SetRetention(ctx, bad); err == nil {
			t.Errorf("%d days accepted", bad)
		}
	}
	old, recent := event(200, time.Millisecond), event(200, time.Millisecond)
	old.At, recent.At = noon.Add(-40*24*time.Hour), noon.Add(-2*24*time.Hour)
	r.Add(old)
	r.Add(recent)
	r.Flush(ctx)
	// Reads never return what is past retention, even before it is pruned.
	got := report(t, r, Query{From: noon.Add(-90 * 24 * time.Hour)})
	if got.Totals.Requests != 1 {
		t.Errorf("past retention was returned: %d", got.Totals.Requests)
	}
	if !got.Range.From.Equal(noon.Truncate(time.Hour).Add(-30 * 24 * time.Hour)) {
		t.Errorf("range.from says %v", got.Range.From)
	}
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows)
	if rows != 1 {
		t.Errorf("prune left %d rows", rows)
	}
	if err := r.SetRetention(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows)
	if rows != 0 {
		t.Errorf("a shorter retention left %d rows", rows)
	}
	// It survives a restart.
	again, err := New(st, r.log)
	if err != nil {
		t.Fatal(err)
	}
	again.now = r.now
	if got := report(t, again, Query{}); got.RetentionDays != 1 {
		t.Errorf("retention after a restart: %d", got.RetentionDays)
	}
}

func TestClear(t *testing.T) {
	r, st := newRecorder(t, nil)
	ctx := context.Background()
	old := event(200, time.Millisecond)
	old.At = noon.Add(-48 * time.Hour)
	r.Add(old)
	r.Flush(ctx)
	r.Add(event(200, time.Millisecond)) // in memory only
	cut := noon.Add(-24 * time.Hour)
	if err := r.Clear(ctx, &cut); err != nil {
		t.Fatal(err)
	}
	if got := report(t, r, Query{From: noon.Add(-72 * time.Hour)}).Totals.Requests; got != 1 {
		t.Errorf("clearing before a date left %d, want the recent one", got)
	}
	if err := r.Clear(ctx, nil); err != nil {
		t.Fatal(err)
	}
	r.Flush(ctx) // the in-memory counts must not come back
	var rows int
	st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows)
	if rows != 0 {
		t.Errorf("%d rows after clearing", rows)
	}
}

// A flush racing a clear must not bring back what was cleared.
func TestClearAndFlushInterleave(t *testing.T) {
	r, st := newRecorder(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 25 {
				r.Add(event(200, time.Millisecond))
				r.Flush(ctx)
			}
		}()
		go func() {
			defer wg.Done()
			for range 25 {
				report(t, r, Query{})
			}
		}()
	}
	wg.Wait()
	if err := r.Clear(ctx, nil); err != nil {
		t.Fatal(err)
	}
	r.Flush(ctx)
	var rows int
	st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows)
	if rows != 0 {
		t.Errorf("%d rows after the last clear", rows)
	}
}

func TestPercentilesWithinTheirBound(t *testing.T) {
	r, _ := newRecorder(t, nil)
	rng := []time.Duration{}
	for i := 1; i <= 2000; i++ {
		// A long-tailed spread from about a millisecond to about a minute.
		rng = append(rng, time.Duration(math.Pow(1.0055, float64(i))*float64(time.Millisecond)))
	}
	for _, d := range rng {
		r.Add(event(200, d))
	}
	sort.Slice(rng, func(i, j int) bool { return rng[i] < rng[j] })
	exact := func(p float64) float64 {
		return float64(rng[int(math.Ceil(p*float64(len(rng))))-1]) / float64(time.Millisecond)
	}
	got := report(t, r, Query{}).Totals.Latency
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{"p50": {got.P50, exact(.5)}, "p95": {got.P95, exact(.95)}, "p99": {got.P99, exact(.99)}} {
		if c.got == nil || math.Abs(*c.got-c.want)/c.want > 0.12 {
			t.Errorf("%s: %v, exact %v", name, c.got, c.want)
		}
	}
}

func TestLatencyEdges(t *testing.T) {
	r, _ := newRecorder(t, nil)
	if got := report(t, r, Query{}).Totals.Latency; got.P50 != nil || !got.Approximate {
		t.Errorf("no requests: %+v", got)
	}
	r.Add(event(200, 0))
	r.Add(event(200, 5*time.Hour)) // past the top bucket
	got := report(t, r, Query{}).Totals.Latency
	if got.P50 == nil || *got.P50 < 0 || got.P99 == nil || *got.P99 <= 0 {
		t.Errorf("edges: %+v", got)
	}
}

// Merging hours gives what one hour would.
func TestMergedHoursMatchOneHour(t *testing.T) {
	one, _ := newRecorder(t, nil)
	two, _ := newRecorder(t, nil)
	for i := range 100 {
		d := time.Duration(i+1) * 37 * time.Millisecond
		one.Add(event(200, d))
		e := event(200, d)
		e.At = noon.Add(-time.Duration(i%5) * time.Hour)
		two.Add(e)
	}
	ctx := context.Background()
	one.Flush(ctx)
	two.Flush(ctx)
	a, b := report(t, one, Query{}).Totals.Latency, report(t, two, Query{}).Totals.Latency
	if *a.P50 != *b.P50 || *a.P95 != *b.P95 || *a.P99 != *b.P99 {
		t.Errorf("%+v against %+v", a, b)
	}
}

func TestGroupsSeriesAndFilters(t *testing.T) {
	r, st := newRecorder(t, nil)
	ctx := context.Background()
	if _, err := st.DB.Exec(`INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at) VALUES ('key_a','my editor','p','h','t','t')`); err != nil {
		t.Fatal(err)
	}
	add := func(kind, id, route, model string, at time.Time, n int) {
		for range n {
			r.Add(Event{Kind: kind, Principal: id, Route: route, Model: model, Status: 200, Duration: time.Second, At: at})
		}
	}
	add("api-key", "key_a", "chat", "m1", noon, 3)
	add("api-key", "key_a", "embeddings", "m2", noon.Add(-time.Hour), 1)
	add("api-key", "key_gone", "chat", "m1", noon, 5)
	add("browser", "browser:abc", "chat", "m1", noon, 2)
	r.Flush(ctx)

	all := report(t, r, Query{Group: []string{"principal"}})
	if all.Totals.Requests != 11 || len(all.Groups) != 3 {
		t.Fatalf("%+v", all)
	}
	if g := all.Groups[0]; g.Principal == nil || g.Principal.ID != "key_gone" || g.Requests != 5 || g.Principal.Name != "" {
		t.Errorf("first by requests, with no name for a key that is gone: %+v", g)
	}
	if g := all.Groups[1]; g.Principal.ID != "key_a" || g.Principal.Name != "my editor" || g.Principal.Kind != "api-key" {
		t.Errorf("a key's name is read when usage is read: %+v", g)
	}
	if g := all.Groups[2]; g.Principal.ID != "browser:abc" || g.Principal.Name != "" {
		t.Errorf("a browser identity has no name: %+v", g)
	}
	if g := all.Groups[0]; g.Model != nil || g.Route != nil {
		t.Errorf("only what was grouped by: %+v", g)
	}
	byModel := report(t, r, Query{Principal: "key_a", Group: []string{"model", "route"}})
	if byModel.Totals.Requests != 4 || len(byModel.Groups) != 2 || *byModel.Groups[0].Model != "m1" || *byModel.Groups[0].Route != "chat" {
		t.Errorf("%+v", byModel)
	}
	if got := report(t, r, Query{Route: "embeddings"}).Totals.Requests; got != 1 {
		t.Errorf("route filter: %d", got)
	}
	if got := report(t, r, Query{Model: "m2"}).Totals.Requests; got != 1 {
		t.Errorf("model filter: %d", got)
	}
	if none := report(t, r, Query{}); len(none.Groups) != 0 {
		t.Errorf("no group: %+v", none.Groups)
	}
	series := report(t, r, Query{}).Series
	if len(series) != 2 || series[0].Requests != 1 || series[1].Requests != 10 || !series[0].T.Before(series[1].T) {
		t.Errorf("series: %+v", series)
	}
	if series[1].P95 == nil {
		t.Errorf("p95: %+v", series[1])
	}
	if got := report(t, r, Query{From: noon.Add(-48 * time.Hour)}).Range.Step; got != "hour" {
		t.Errorf("step %s", got)
	}
	if got := report(t, r, Query{From: noon.Add(-20 * 24 * time.Hour)}); got.Range.Step != "day" || len(got.Series) != 1 {
		t.Errorf("day step: %+v", got)
	}
}

func TestParseRange(t *testing.T) {
	now := noon
	for _, c := range []struct {
		rng, from, to string
		ok            bool
		span          time.Duration
	}{
		{"", "", "", true, 7 * 24 * time.Hour},
		{"24h", "", "", true, 24 * time.Hour},
		{"7d", "", "", true, 7 * 24 * time.Hour},
		{"30d", "", "", true, 30 * 24 * time.Hour},
		{"1y", "", "", false, 0},
		{"", "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z", true, 24 * time.Hour},
		{"", "nonsense", "", false, 0},
		{"", "2026-11-02T00:00:00Z", "2026-11-01T00:00:00Z", false, 0},
		{"7d", "2026-11-01T00:00:00Z", "", false, 0},
	} {
		from, to, err := ParseRange(c.rng, c.from, c.to, now)
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		if err == nil && to.Sub(from) != c.span {
			t.Errorf("%+v: %v to %v", c, from, to)
		}
	}
}

// Nothing here can reach a network or a collector (DESIGN §32.1).
func TestImports(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "net" || strings.HasPrefix(path, "net/") || strings.HasSuffix(path, "internal/otlp") || strings.HasSuffix(path, "internal/obs") {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

// closedStore is a store that fails every statement, as a full or locked disk would.
func closedStore(t *testing.T) *store.Store {
	t.Helper()
	bad, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	bad.Close()
	return bad
}

func TestStartSaysWhenTheFirstPruneFails(t *testing.T) {
	var logs bytes.Buffer
	r, _ := newRecorder(t, &logs)
	r.st = closedStore(t)
	r.Start()() // stop waits for the goroutine, so reading the logs after this is safe
	if !strings.Contains(logs.String(), "usage.prune-failed") {
		t.Errorf("a prune that failed on start left no line: %q", logs.String())
	}
}

// Flush's own line says the counts are kept in memory. On the way out that stops being true.
func TestStopSaysWhenTheLastWriteLosesCounts(t *testing.T) {
	var logs bytes.Buffer
	r, _ := newRecorder(t, &logs)
	r.Add(event(200, time.Millisecond))
	r.st = closedStore(t)
	r.Start()()
	if !strings.Contains(logs.String(), "usage.last-flush-failed") {
		t.Errorf("counts lost on stop left no line: %q", logs.String())
	}
}

func TestStopIsQuietWhenTheLastWriteWorks(t *testing.T) {
	var logs bytes.Buffer
	r, _ := newRecorder(t, &logs)
	r.Add(event(200, time.Millisecond))
	r.Start()()
	if strings.Contains(logs.String(), "failed") {
		t.Errorf("nothing failed, but: %q", logs.String())
	}
}
