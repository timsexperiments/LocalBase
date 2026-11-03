// Package usage keeps hourly totals of inference requests (DESIGN §32): counts, token
// totals and durations, per principal, route, model and status. It never holds a
// prompt, a reply, a body, a file name or a credential, and it has no way to send
// anything anywhere: TestImports keeps network and telemetry packages out.
package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	// DefaultRetentionDays is how long totals are kept unless changed.
	DefaultRetentionDays = 30
	minRetentionDays     = 1
	maxRetentionDays     = 365

	retentionKey = "usage.retention-days"
	recordingKey = "usage.recording"

	// FlushInterval bounds what a crash can lose.
	FlushInterval = 30 * time.Second
	pruneInterval = time.Hour

	// Latency buckets are 25% apart, from 1 ms to about an hour: a value reads back
	// within about 12% (DESIGN §32.3).
	bucketFactor = 1.25
	topBucket    = 68
)

// Routes is the fixed list of what can be counted. Nothing else is stored as a route.
var Routes = []string{"chat", "embeddings", "decision", "speech", "transcription", "translation", "image", "image-edit", "video"}

// CanSeeAll says who may read everything, by a caller's permission check (DESIGN §32.5).
func CanSeeAll(can func(string) bool) bool { return can("models:manage") || can("system:read") }

// CanChange says who may clear and change settings (DESIGN §32.5). access:manage can grant
// itself system:manage anyway, so allowing it escalates nothing.
func CanChange(can func(string) bool) bool { return can("system:manage") || can("access:manage") }

// Event is one finished request. The caller vouches that Model is an id the gateway
// knows, or empty.
type Event struct {
	Kind, Principal           string
	Route, Model              string
	Status                    int
	Duration, QueueWait       time.Duration
	InputTokens, OutputTokens int64
	HasTokens                 bool
	At                        time.Time
}

type rowKey struct {
	hour            int64
	kind, principal string
	route, model    string
	status          int
}

type totals struct {
	requests, withTokens, input, output, durationMs, queueMs int64
	latency                                                  map[int]int64
}

func (t *totals) merge(o *totals) {
	t.requests += o.requests
	t.withTokens += o.withTokens
	t.input += o.input
	t.output += o.output
	t.durationMs += o.durationMs
	t.queueMs += o.queueMs
	if t.latency == nil {
		t.latency = map[int]int64{}
	}
	for b, n := range o.latency {
		t.latency[b] += n
	}
}

// Recorder counts in memory and writes hourly totals. Only serve writes them.
type Recorder struct {
	st  *store.Store
	log *slog.Logger
	now func() time.Time

	recording atomic.Bool
	retention atomic.Int64

	mu      sync.Mutex
	pending map[rowKey]*totals

	// io keeps a flush and a clear from interleaving, and a read from seeing counts
	// that have left memory but not reached the database.
	io sync.RWMutex
}

// New reads the settings. Recording is on and retention is 30 days unless changed.
func New(st *store.Store, log *slog.Logger) (*Recorder, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Recorder{st: st, log: log, now: time.Now, pending: map[rowKey]*totals{}}
	ctx := context.Background()
	on, set, err := st.Setting(ctx, recordingKey)
	if err != nil {
		return nil, err
	}
	r.recording.Store(!set || on != "off")
	days, set, err := st.Setting(ctx, retentionKey)
	if err != nil {
		return nil, err
	}
	r.retention.Store(DefaultRetentionDays)
	if set {
		if n, err := strconv.Atoi(days); err == nil && n >= minRetentionDays && n <= maxRetentionDays {
			r.retention.Store(int64(n))
		}
	}
	return r, nil
}

func bucket(d time.Duration) int {
	ms := float64(d) / float64(time.Millisecond)
	if ms <= 1 {
		return 0
	}
	return min(int(math.Ceil(math.Log(ms)/math.Log(bucketFactor))), topBucket)
}

// bucketMs is a bucket's middle, in ms: at most 12% from anything in it.
func bucketMs(b int) float64 {
	if b <= 0 {
		return 1
	}
	return math.Pow(bucketFactor, float64(b)-0.5)
}

// Add counts one request. It never blocks on the database.
func (r *Recorder) Add(e Event) {
	if r == nil || !r.recording.Load() || !slices.Contains(Routes, e.Route) {
		return
	}
	at := e.At
	if at.IsZero() {
		at = r.now()
	}
	key := rowKey{at.UTC().Truncate(time.Hour).Unix(), e.Kind, e.Principal, e.Route, e.Model, e.Status}
	d := max(e.Duration, 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.pending[key]
	if t == nil {
		t = &totals{latency: map[int]int64{}}
		r.pending[key] = t
	}
	t.requests++
	if e.HasTokens {
		t.withTokens++
		t.input += e.InputTokens
		t.output += e.OutputTokens
	}
	t.durationMs += d.Milliseconds()
	t.queueMs += max(e.QueueWait, 0).Milliseconds()
	t.latency[bucket(d)]++
}

// Flush writes what is in memory in one transaction. A failure keeps the counts, logs
// usage.flush-failed, and never reaches a request.
func (r *Recorder) Flush(ctx context.Context) error {
	r.io.Lock()
	defer r.io.Unlock()
	r.mu.Lock()
	taken := r.pending
	r.pending = map[rowKey]*totals{}
	r.mu.Unlock()
	if len(taken) == 0 {
		return nil
	}
	err := r.write(ctx, taken)
	if err != nil {
		r.mu.Lock()
		for k, t := range taken {
			if cur := r.pending[k]; cur != nil {
				t.merge(cur)
			}
			r.pending[k] = t
		}
		r.mu.Unlock()
		r.log.Warn("Could not write usage totals; they are kept in memory.", "event", "usage.flush-failed", "error", err.Error())
	}
	return err
}

func (r *Recorder) write(ctx context.Context, rows map[rowKey]*totals) error {
	tx, err := r.st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, t := range rows {
		merged := &totals{latency: map[int]int64{}}
		merged.merge(t)
		var hist string
		err := tx.QueryRowContext(ctx, `SELECT latency FROM usage_hourly WHERE hour=? AND principal_kind=? AND principal_id=? AND route=? AND model_id=? AND status=?`,
			k.hour, k.kind, k.principal, k.route, k.model, k.status).Scan(&hist)
		switch {
		case err == nil:
			var old map[int]int64
			if json.Unmarshal([]byte(hist), &old) == nil {
				merged.merge(&totals{latency: old})
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		b, err := json.Marshal(merged.latency)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_hourly
			(hour, principal_kind, principal_id, route, model_id, status, requests, with_tokens, input_tokens, output_tokens, duration_ms, queue_ms, latency)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (hour, principal_kind, principal_id, route, model_id, status) DO UPDATE SET
			requests = requests + excluded.requests, with_tokens = with_tokens + excluded.with_tokens,
			input_tokens = input_tokens + excluded.input_tokens, output_tokens = output_tokens + excluded.output_tokens,
			duration_ms = duration_ms + excluded.duration_ms, queue_ms = queue_ms + excluded.queue_ms, latency = excluded.latency`,
			k.hour, k.kind, k.principal, k.route, k.model, k.status, t.requests, t.withTokens, t.input, t.output, t.durationMs, t.queueMs, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// floor is the oldest hour retention allows.
func (r *Recorder) floor() time.Time {
	return r.now().UTC().Truncate(time.Hour).Add(-time.Duration(r.retention.Load()) * 24 * time.Hour)
}

// Prune removes what is past retention.
func (r *Recorder) Prune(ctx context.Context) error {
	r.io.Lock()
	defer r.io.Unlock()
	_, err := r.st.DB.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour < ?`, r.floor().Unix())
	return err
}

// Clear removes every total, or only those before a time, here and in memory.
func (r *Recorder) Clear(ctx context.Context, before *time.Time) error {
	r.io.Lock()
	defer r.io.Unlock()
	r.mu.Lock()
	for k := range r.pending {
		if before == nil || k.hour < before.Unix() {
			delete(r.pending, k)
		}
	}
	r.mu.Unlock()
	var err error
	if before == nil {
		_, err = r.st.DB.ExecContext(ctx, `DELETE FROM usage_hourly`)
	} else {
		_, err = r.st.DB.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour < ?`, before.Unix())
	}
	return err
}

// InvalidRetention is a retention outside what is allowed.
type InvalidRetention struct{ Days int }

func (e *InvalidRetention) Error() string {
	return fmt.Sprintf("retention must be from %d to %d days", minRetentionDays, maxRetentionDays)
}

// Settings says whether counting is on, and for how many days totals are kept.
func (r *Recorder) Settings() (recording bool, retentionDays int) {
	return r.recording.Load(), int(r.retention.Load())
}

// SetRetention keeps totals for this many days.
func (r *Recorder) SetRetention(ctx context.Context, days int) error {
	if days < minRetentionDays || days > maxRetentionDays {
		return &InvalidRetention{days}
	}
	if err := r.st.SetSetting(ctx, retentionKey, strconv.Itoa(days)); err != nil {
		return err
	}
	r.retention.Store(int64(days))
	return nil
}

// SetRecording turns counting on or off. What exists stays until it is cleared or ages out.
func (r *Recorder) SetRecording(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	if err := r.st.SetSetting(ctx, recordingKey, value); err != nil {
		return err
	}
	r.recording.Store(on)
	return nil
}

// Start writes every 30 s and prunes hourly. The returned function stops it after one last write.
func (r *Recorder) Start() (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		flush, prune := time.NewTicker(FlushInterval), time.NewTicker(pruneInterval)
		defer flush.Stop()
		defer prune.Stop()
		r.Prune(context.Background())
		for {
			select {
			case <-flush.C:
				r.Flush(context.Background())
			case <-prune.C:
				if err := r.Prune(context.Background()); err != nil {
					r.log.Warn("Could not prune usage totals.", "event", "usage.prune-failed", "error", err.Error())
				}
			case <-quit:
				r.Flush(context.Background())
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(quit); <-done })
	}
}

// --- reading -------------------------------------------------------------------------

// Query says what to report. Principal narrows to one id; From and To are required.
type Query struct {
	From, To                time.Time
	Principal, Model, Route string
	Group                   []string // principal, model, route
}

// Latency is approximate by construction. With no requests its figures are null.
type Latency struct {
	P50         *float64 `json:"p50"`
	P95         *float64 `json:"p95"`
	P99         *float64 `json:"p99"`
	Approximate bool     `json:"approximate"`
}

// Stats are the figures every level of a report has.
type Stats struct {
	Requests           int64   `json:"requests"`
	Failed             int64   `json:"failed"`
	Refused            int64   `json:"refused"`
	Cancelled          int64   `json:"cancelled"`
	ErrorRate          float64 `json:"errorRate"`
	InputTokens        int64   `json:"inputTokens"`
	OutputTokens       int64   `json:"outputTokens"`
	RequestsWithTokens int64   `json:"requestsWithTokens"`
	Latency            Latency `json:"latencyMs"`
	QueueWaitMsAvg     float64 `json:"queueWaitMsAvg"`
}

// Principal names who a group is. Name is present only for a key that still exists.
type Principal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Group is one line of a breakdown, with only the fields grouped by.
type Group struct {
	Principal *Principal `json:"principal,omitempty"`
	Model     *string    `json:"model,omitempty"`
	Route     *string    `json:"route,omitempty"`
	Stats
}

// Point is one step of the series. A step with no requests is left out.
type Point struct {
	T            time.Time `json:"t"`
	Requests     int64     `json:"requests"`
	Failed       int64     `json:"failed"`
	Refused      int64     `json:"refused"`
	Cancelled    int64     `json:"cancelled"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	P95          *float64  `json:"p95Ms"`
}

// Span says what a report covers, after retention.
type Span struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Step string    `json:"step"`
}

// Report is what /_localbase/usage answers.
type Report struct {
	Object        string  `json:"object"`
	Scope         string  `json:"scope"`
	Range         Span    `json:"range"`
	Recording     bool    `json:"recording"`
	RetentionDays int     `json:"retentionDays"`
	Totals        Stats   `json:"totals"`
	Groups        []Group `json:"groups"`
	Series        []Point `json:"series"`
}

// ParseRange reads 24h, 7d or 30d (default 7d), or explicit RFC 3339 times, not both.
func ParseRange(rng, from, to string, now time.Time) (time.Time, time.Time, error) {
	end := now.UTC().Truncate(time.Hour).Add(time.Hour)
	if from != "" || to != "" {
		if rng != "" {
			return time.Time{}, time.Time{}, errors.New("give range, or from and to, not both")
		}
		start := end.Add(-7 * 24 * time.Hour)
		var err error
		if from != "" {
			if start, err = time.Parse(time.RFC3339, from); err != nil {
				return time.Time{}, time.Time{}, errors.New("from must be an RFC 3339 time")
			}
		}
		if to != "" {
			if end, err = time.Parse(time.RFC3339, to); err != nil {
				return time.Time{}, time.Time{}, errors.New("to must be an RFC 3339 time")
			}
		}
		if !start.Before(end) {
			return time.Time{}, time.Time{}, errors.New("from must be before to")
		}
		return start.UTC(), end.UTC(), nil
	}
	span := map[string]time.Duration{"": 7 * 24 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}
	d, ok := span[rng]
	if !ok {
		return time.Time{}, time.Time{}, errors.New("range must be 24h, 7d or 30d")
	}
	return end.Add(-d), end, nil
}

type accum struct {
	Stats
	durationQueue int64
	latency       map[int]int64
}

func (a *accum) add(status int, t *totals) {
	a.Requests += t.requests
	switch {
	case status == 499:
		a.Cancelled += t.requests
	case status >= 500:
		a.Failed += t.requests
	case status >= 400:
		a.Refused += t.requests
	}
	a.InputTokens += t.input
	a.OutputTokens += t.output
	a.RequestsWithTokens += t.withTokens
	a.durationQueue += t.queueMs
	if a.latency == nil {
		a.latency = map[int]int64{}
	}
	for b, n := range t.latency {
		a.latency[b] += n
	}
}

func percentile(hist map[int]int64, total int64, p float64) *float64 {
	if total <= 0 {
		return nil
	}
	rank := int64(math.Ceil(p * float64(total)))
	buckets := make([]int, 0, len(hist))
	for b := range hist {
		buckets = append(buckets, b)
	}
	sort.Ints(buckets)
	var seen int64
	for _, b := range buckets {
		if seen += hist[b]; seen >= rank {
			v := bucketMs(b)
			return &v
		}
	}
	return nil
}

func (a *accum) finish() Stats {
	s := a.Stats
	s.Latency = Latency{Approximate: true}
	if s.Requests > 0 {
		s.ErrorRate = float64(s.Failed+s.Refused) / float64(s.Requests)
		s.QueueWaitMsAvg = float64(a.durationQueue) / float64(s.Requests)
		s.Latency.P50 = percentile(a.latency, s.Requests, 0.50)
		s.Latency.P95 = percentile(a.latency, s.Requests, 0.95)
		s.Latency.P99 = percentile(a.latency, s.Requests, 0.99)
	}
	return s
}

// Report answers a query from the database and what is still in memory.
func (r *Recorder) Report(ctx context.Context, q Query) (*Report, error) {
	r.io.RLock()
	defer r.io.RUnlock()
	from, to := q.From, q.To
	if floor := r.floor(); from.Before(floor) {
		from = floor
	}
	out := &Report{Object: "localbase.usage", Scope: "self", Recording: r.recording.Load(), RetentionDays: int(r.retention.Load()),
		Groups: []Group{}, Series: []Point{}}
	step, stepName := time.Hour, "hour"
	if to.Sub(from) > 7*24*time.Hour+time.Hour {
		step, stepName = 24*time.Hour, "day"
	}
	out.Range = Span{From: from, To: to, Step: stepName}

	// The database, then what has not reached it.
	rows := map[rowKey]*totals{}
	list, err := r.st.DB.QueryContext(ctx, `SELECT hour, principal_kind, principal_id, route, model_id, status, requests, with_tokens,
		input_tokens, output_tokens, duration_ms, queue_ms, latency FROM usage_hourly WHERE hour >= ? AND hour < ?`,
		from.Truncate(time.Hour).Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer list.Close()
	for list.Next() {
		var k rowKey
		t := &totals{}
		var hist string
		if err := list.Scan(&k.hour, &k.kind, &k.principal, &k.route, &k.model, &k.status, &t.requests, &t.withTokens,
			&t.input, &t.output, &t.durationMs, &t.queueMs, &hist); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hist), &t.latency); err != nil {
			t.latency = map[int]int64{}
		}
		rows[k] = t
	}
	if err := list.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	for k, t := range r.pending {
		if k.hour < from.Truncate(time.Hour).Unix() || k.hour >= to.Unix() {
			continue
		}
		if cur := rows[k]; cur != nil {
			cur.merge(t)
		} else {
			c := &totals{latency: map[int]int64{}}
			c.merge(t)
			rows[k] = c
		}
	}
	r.mu.Unlock()

	var all accum
	groups := map[string]*struct {
		g Group
		a accum
	}{}
	points := map[int64]*accum{}
	for k, t := range rows {
		if (q.Principal != "" && k.principal != q.Principal) || (q.Model != "" && k.model != q.Model) || (q.Route != "" && k.route != q.Route) {
			continue
		}
		all.add(k.status, t)
		slot := time.Unix(k.hour, 0).UTC().Truncate(step).Unix()
		if points[slot] == nil {
			points[slot] = &accum{}
		}
		points[slot].add(k.status, t)
		if len(q.Group) == 0 {
			continue
		}
		var g Group
		id := ""
		if slices.Contains(q.Group, "principal") {
			g.Principal = &Principal{Kind: k.kind, ID: k.principal}
			id += k.kind + "\x00" + k.principal
		}
		id += "\x00"
		if slices.Contains(q.Group, "model") {
			m := k.model
			g.Model, id = &m, id+m
		}
		id += "\x00"
		if slices.Contains(q.Group, "route") {
			rt := k.route
			g.Route, id = &rt, id+rt
		}
		e := groups[id]
		if e == nil {
			e = &struct {
				g Group
				a accum
			}{g: g}
			groups[id] = e
		}
		e.a.add(k.status, t)
	}
	out.Totals = all.finish()
	names := r.keyNames(ctx, q.Group)
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := groups[id]
		e.g.Stats = e.a.finish()
		if e.g.Principal != nil && e.g.Principal.Kind == "api-key" {
			e.g.Principal.Name = names[e.g.Principal.ID]
		}
		out.Groups = append(out.Groups, e.g)
	}
	sort.SliceStable(out.Groups, func(i, j int) bool { return out.Groups[i].Requests > out.Groups[j].Requests })
	slots := make([]int64, 0, len(points))
	for s := range points {
		slots = append(slots, s)
	}
	slices.Sort(slots)
	for _, s := range slots {
		f := points[s].finish()
		out.Series = append(out.Series, Point{T: time.Unix(s, 0).UTC(), Requests: f.Requests, Failed: f.Failed, Refused: f.Refused,
			Cancelled: f.Cancelled, InputTokens: f.InputTokens, OutputTokens: f.OutputTokens, P95: f.Latency.P95})
	}
	return out, nil
}

// keyNames reads keys' labels when usage is read. The table holds ids only.
func (r *Recorder) keyNames(ctx context.Context, group []string) map[string]string {
	if !slices.Contains(group, "principal") {
		return nil
	}
	out := map[string]string{}
	list, err := r.st.DB.QueryContext(ctx, `SELECT id, name FROM api_keys`)
	if err != nil {
		return out
	}
	defer list.Close()
	for list.Next() {
		var id, name string
		if list.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}
