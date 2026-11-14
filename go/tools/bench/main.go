// Command bench measures what the gateway itself costs, against fake backends: added
// latency per request and per streamed token, memory per idle connection, how key
// lookup scales, startup, the idle footprint and SQLite contention.
//
//	make bench                      measure and write a result table
//	make bench-compare              the same, then compare with the recorded baseline
//
// It compares with a baseline recorded on the same kind of machine, and reports a
// regression only past a generous margin: this is a guard for a nightly job, not a
// gate on pull requests. See docs/performance.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// metric is one measured figure. Lower is better for all of them.
type metric struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
	// Budget is the figure this must stay under on the machine the baseline was
	// recorded on (0: none).
	Budget float64 `json:"budget,omitempty"`
}

type result struct {
	Platform string   `json:"platform"`
	Commit   string   `json:"commit"`
	Metrics  []metric `json:"metrics"`
	// Load is the machine's one-minute load average, since a figure from a busy
	// machine is not comparable to one from a quiet one.
	Load string `json:"load"`
}

type bench struct {
	w       *lab.Workspace
	ctx     context.Context
	quick   bool
	metrics []metric
}

func (b *bench) add(name, unit string, value float64) {
	b.metrics = append(b.metrics, metric{Name: name, Unit: unit, Value: value, Budget: budgets[name]})
	fmt.Fprintf(os.Stderr, "bench: %-42s %12.2f %s\n", name, value, unit)
}

func (b *bench) samples(full, quick int) int {
	if b.quick {
		return quick
	}
	return full
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func main() {
	out := flag.String("out", "", "result directory (default: a new directory under $TMPDIR/localbase-bench)")
	compare := flag.String("compare", "", "baseline JSON to compare with; exit 1 on a regression")
	writeBaseline := flag.String("write-baseline", "", "write the result here as the new baseline")
	quick := flag.Bool("quick", false, "fewer samples, for checking the tool itself")
	flag.Parse()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	goruntime.GOMAXPROCS(2)
	os.Exit(run(*out, *compare, *writeBaseline, *quick))
}

func run(out, compare, baseline string, quick bool) int {
	if out == "" {
		out = filepath.Join(os.TempDir(), "localbase-bench", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	const hard = 10 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), hard)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	w, err := lab.NewWorkspace("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	defer w.Close()
	time.AfterFunc(hard+30*time.Second, func() {
		fmt.Fprintln(os.Stderr, "bench: hard deadline passed; stopping everything")
		w.KillStray()
		os.Exit(3)
	})
	if err := w.Build(ctx, false); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	b := &bench{w: w, ctx: ctx, quick: quick}
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"binary size", b.binarySize},
		{"startup and idle footprint", b.startupAndIdle},
		{"overhead per request", b.overheadPerRequest},
		{"overhead per streamed token", b.overheadPerToken},
		{"memory per idle connection", b.idleConnections},
		{"key lookup scaling", b.keyScaling},
		{"SQLite contention", b.contention},
	} {
		if ctx.Err() != nil {
			return 3
		}
		fmt.Fprintf(os.Stderr, "bench: %s\n", step.name)
		if err := step.do(); err != nil {
			fmt.Fprintf(os.Stderr, "bench: %s: %v\n", step.name, err)
			return 2
		}
		w.KillStray()
	}
	res := result{Platform: goruntime.GOOS + "/" + goruntime.GOARCH, Commit: gitCommit(), Metrics: b.metrics, Load: loadAverage()}
	if err := writeJSON(filepath.Join(out, "result.json"), res); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	var regressions []string
	if compare != "" {
		var base result
		raw, err := os.ReadFile(compare)
		switch {
		case os.IsNotExist(err):
			fmt.Fprintf(os.Stderr, "bench: no baseline at %s; nothing to compare with\n", compare)
		case err != nil:
			fmt.Fprintln(os.Stderr, "bench:", err)
			return 2
		default:
			if err := json.Unmarshal(raw, &base); err != nil {
				fmt.Fprintln(os.Stderr, "bench:", err)
				return 2
			}
			regressions = compareResults(base, res)
		}
	}
	md := markdown(res, compare, regressions)
	if err := os.WriteFile(filepath.Join(out, "result.md"), []byte(md), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 2
	}
	fmt.Println(md)
	fmt.Fprintf(os.Stderr, "bench: result %s\n", filepath.Join(out, "result.md"))
	if baseline != "" {
		if err := writeJSON(baseline, res); err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			return 2
		}
	}
	if len(regressions) > 0 {
		return 1
	}
	return 0
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// start runs a gateway with the fake backend answering at once.
func (b *bench) start(name string) (*lab.Gateway, *lab.Client, error) {
	g, err := b.w.Start(b.ctx, lab.Options{Name: name, CtxSize: 8192, QueueCapacity: 64, FakeDelayMS: 0})
	if err != nil {
		return nil, nil, err
	}
	return g, lab.NewClient(g.Base), nil
}

func stopGateway(g *lab.Gateway) {
	if _, err := g.Stop(30 * time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "bench: the gateway did not stop cleanly:", err)
	}
}

func (b *bench) binarySize() error {
	info, err := os.Stat(filepath.Join(b.w.Bin, "local-base"))
	if err != nil {
		return err
	}
	// The lab build adds a few handlers; a release differs by very little.
	b.add("binary size", "MiB", float64(info.Size())/(1<<20))
	return nil
}

func (b *bench) startupAndIdle() error {
	g, _, err := b.start("startup")
	if err != nil {
		return err
	}
	b.add("startup, new root", "ms", ms(g.Startup))
	var restarts []time.Duration
	for range b.samples(7, 3) {
		if _, err := g.Stop(30 * time.Second); err != nil {
			return err
		}
		next, err := b.w.Start(b.ctx, lab.Options{Name: "startup", Reuse: g, CtxSize: 8192, QueueCapacity: 64})
		if err != nil {
			return err
		}
		restarts = append(restarts, next.Startup)
		g = next
	}
	defer stopGateway(g)
	b.add("startup, existing root", "ms", ms(lab.Percentile(lab.Sorted(restarts), 50)))
	c := lab.NewClient(g.Base)
	// Idle: running, with no model loaded and no client connected.
	time.Sleep(time.Second)
	s, err := g.Settled(b.ctx)
	if err != nil {
		return err
	}
	b.add("idle RSS", "MiB", float64(s.RSS)/(1<<20))
	b.add("idle heap in use", "MiB", float64(s.HeapInuse)/(1<<20))
	b.add("idle goroutines", "count", float64(s.Goroutines))
	b.add("idle file descriptors", "count", float64(s.FDs))
	// The first request loads the model: what a client waits for after a cold start.
	began := time.Now()
	r := c.PostJSON(b.ctx, "/v1/chat/completions", g.Keys.User, chatBody(lab.ChatSmall, "hello"))
	if r.Err != nil || r.Status != 200 {
		return fmt.Errorf("first request: %d %v %.200s", r.Status, r.Err, r.Body)
	}
	b.add("first request, model load included", "ms", ms(time.Since(began)))
	return nil
}

func chatBody(model, content string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": content}}}
}

// overheadPerRequest alternates the same request straight to the backend and through
// the gateway, so whatever else the machine is doing lands on both alike.
func (b *bench) overheadPerRequest() error {
	g, gw, err := b.start("overhead")
	if err != nil {
		return err
	}
	defer stopGateway(g)
	body := chatBody(lab.ChatSmall, "hello")
	if r := gw.PostJSON(b.ctx, "/v1/chat/completions", g.Keys.User, body); r.Status != 200 {
		return fmt.Errorf("loading: %d %.200s", r.Status, r.Body)
	}
	direct := lab.NewClient(g.LLMBase())
	n := b.samples(2000, 200)
	var viaD, directD []time.Duration
	cpuBefore := lab.CPUSeconds(g.Cmd.Process.Pid)
	for range n {
		d := direct.PostJSON(b.ctx, "/v1/chat/completions", "", body)
		if d.Err != nil || d.Status != 200 {
			return fmt.Errorf("direct: %d %v %.200s", d.Status, d.Err, d.Body)
		}
		v := gw.PostJSON(b.ctx, "/v1/chat/completions", g.Keys.User, body)
		if v.Err != nil || v.Status != 200 {
			return fmt.Errorf("gateway: %d %v %.200s", v.Status, v.Err, v.Body)
		}
		directD, viaD = append(directD, d.Dur), append(viaD, v.Dur)
	}
	b.add("gateway CPU per request", "µs", (lab.CPUSeconds(g.Cmd.Process.Pid)-cpuBefore)*1e6/float64(n))
	b.add("backend directly, p50", "ms", ms(lab.Percentile(lab.Sorted(directD), 50)))
	b.add("through the gateway, p50", "ms", ms(lab.Percentile(lab.Sorted(viaD), 50)))
	b.add("added per request, p50", "ms", ms(lab.Percentile(lab.Sorted(viaD), 50)-lab.Percentile(lab.Sorted(directD), 50)))
	b.add("added per request, p99", "ms", ms(lab.Percentile(lab.Sorted(viaD), 99)-lab.Percentile(lab.Sorted(directD), 99)))
	return nil
}

// overheadPerToken streams with no pause in the backend, so what is left is the cost
// of carrying each chunk. Time alone understates that: the backend, the gateway and
// the client run at once, so a stage's work hides unless it is the slowest. The
// gateway's CPU per token is what relaying really costs.
func (b *bench) overheadPerToken() error {
	g, gw, err := b.start("stream")
	if err != nil {
		return err
	}
	defer stopGateway(g)
	const tokens = 500
	body := chatBody(lab.ChatSmall, fmt.Sprintf("fake:burst %d", tokens))
	body["stream"] = true
	if st := gw.ChatStream(b.ctx, g.Keys.User, body, nil); st.Err != nil || !st.Done {
		return fmt.Errorf("loading: %d %v", st.Status, st.Err)
	}
	direct := lab.NewClient(g.LLMBase())
	n := b.samples(100, 15)
	var viaTotal, directTotal, viaFirst, directFirst []time.Duration
	cpuBefore := lab.CPUSeconds(g.Cmd.Process.Pid)
	for range n {
		d := direct.ChatStream(b.ctx, "", body, nil)
		if d.Err != nil || !d.Done || d.Chunks < tokens {
			return fmt.Errorf("direct stream: %d chunks, %v", d.Chunks, d.Err)
		}
		v := gw.ChatStream(b.ctx, g.Keys.User, body, nil)
		if v.Err != nil || !v.Done || v.Chunks < tokens {
			return fmt.Errorf("gateway stream: %d chunks, %v", v.Chunks, v.Err)
		}
		directTotal, viaTotal = append(directTotal, d.Dur), append(viaTotal, v.Dur)
		directFirst, viaFirst = append(directFirst, d.FirstToken), append(viaFirst, v.FirstToken)
	}
	b.add("gateway CPU per streamed token", "µs", (lab.CPUSeconds(g.Cmd.Process.Pid)-cpuBefore)*1e6/float64(n*tokens))
	addedTotal := lab.Percentile(lab.Sorted(viaTotal), 50) - lab.Percentile(lab.Sorted(directTotal), 50)
	b.add("added per streamed token, p50", "µs", float64(addedTotal)/float64(time.Microsecond)/tokens)
	b.add("added to first token, p50", "ms", ms(lab.Percentile(lab.Sorted(viaFirst), 50)-lab.Percentile(lab.Sorted(directFirst), 50)))
	b.add("added to first token, p99", "ms", ms(lab.Percentile(lab.Sorted(viaFirst), 99)-lab.Percentile(lab.Sorted(directFirst), 99)))
	return nil
}

// idleConnections holds kept-alive connections open, each having made one request.
func (b *bench) idleConnections() error {
	g, _, err := b.start("idle")
	if err != nil {
		return err
	}
	defer stopGateway(g)
	before, err := g.Settled(b.ctx)
	if err != nil {
		return err
	}
	n := b.samples(500, 100)
	conns := make([]net.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	request := fmt.Sprintf("GET /health HTTP/1.1\r\nHost: %s\r\n\r\n", g.Addr())
	buf := make([]byte, 4096)
	for i := range n {
		c, err := net.DialTimeout("tcp", g.Addr(), 5*time.Second)
		if err != nil {
			return fmt.Errorf("connection %d: %w", i, err)
		}
		conns = append(conns, c)
		if _, err := io.WriteString(c, request); err != nil {
			return err
		}
		if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := c.Read(buf); err != nil {
			return fmt.Errorf("connection %d: %w", i, err)
		}
	}
	after, err := g.Settled(b.ctx)
	if err != nil {
		return err
	}
	per := func(x, y int64) float64 { return float64(y-x) / float64(n) }
	b.add("memory per idle connection, RSS", "KiB", per(before.RSS, after.RSS)/1024)
	b.add("memory per idle connection, heap", "KiB", per(before.HeapInuse, after.HeapInuse)/1024)
	b.add("goroutines per idle connection", "count", per(int64(before.Goroutines), int64(after.Goroutines)))
	return nil
}

// keyScaling times an authenticated request that does almost nothing else, as the
// number of keys grows. Key lookup reads every key on each request.
func (b *bench) keyScaling() error {
	g, c, err := b.start("keys")
	if err != nil {
		return err
	}
	defer stopGateway(g)
	st, err := store.Open(filepath.Join(g.Root, "local-base.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	sample := func() ([]time.Duration, error) {
		n := b.samples(500, 100)
		out := make([]time.Duration, 0, n)
		for range n {
			r := c.Do(b.ctx, "GET", "/_localbase/runs", g.Keys.User, "", nil)
			if r.Err != nil || r.Status != 200 {
				return nil, fmt.Errorf("authenticated request: %d %v %.200s", r.Status, r.Err, r.Body)
			}
			out = append(out, r.Dur)
		}
		return out, nil
	}
	keys, err := st.ListAPIKeys(b.ctx)
	if err != nil {
		return err
	}
	own := store.HashKey(g.Keys.User)
	for _, k := range keys {
		if k.KeyHash != own {
			if err := st.DeleteAPIKey(b.ctx, k.ID); err != nil {
				return err
			}
		}
	}
	count := 1
	for _, level := range []int{1, 100, 1000} {
		for ; count < level; count++ {
			if _, _, err := st.CreateAPIKey(b.ctx, fmt.Sprintf("bench-%d", count), []string{"models:read"}); err != nil {
				return err
			}
		}
		d, err := sample()
		if err != nil {
			return err
		}
		s := lab.Sorted(d)
		b.add(fmt.Sprintf("auth with %d keys, p50", level), "ms", ms(lab.Percentile(s, 50)))
		b.add(fmt.Sprintf("auth with %d keys, p99", level), "ms", ms(lab.Percentile(s, 99)))
	}
	return nil
}

// contention times reads while writers update a key as fast as they can: every
// request reads the database, and SQLite here has one writer at a time.
func (b *bench) contention() error {
	g, c, err := b.start("contention")
	if err != nil {
		return err
	}
	defer stopGateway(g)
	window := 8 * time.Second
	if b.quick {
		window = 2 * time.Second
	}
	reads := func(limit time.Duration) ([]time.Duration, error) {
		var out []time.Duration
		for deadline := time.Now().Add(limit); time.Now().Before(deadline); {
			r := c.Do(b.ctx, "GET", "/_localbase/runs", g.Keys.User, "", nil)
			if r.Err != nil || r.Status != 200 {
				return nil, fmt.Errorf("read: %d %v %.200s", r.Status, r.Err, r.Body)
			}
			out = append(out, r.Dur)
			time.Sleep(2 * time.Millisecond)
		}
		return out, nil
	}
	quiet, err := reads(window)
	if err != nil {
		return err
	}
	list := c.Do(b.ctx, "GET", "/_localbase/api-keys", g.Keys.Admin, "", nil)
	var keys struct {
		Keys []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"keys"`
	}
	if list.Status != 200 || json.Unmarshal(list.Body, &keys) != nil {
		return fmt.Errorf("listing keys: %d %.200s", list.Status, list.Body)
	}
	var targets []string
	for _, k := range keys.Keys {
		if strings.HasPrefix(k.Name, "lab-runs-") {
			targets = append(targets, k.ID)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no keys to update")
	}
	writeCtx, stopWriters := context.WithCancel(b.ctx)
	defer stopWriters()
	var wrote, failed atomic.Int64
	var wg sync.WaitGroup
	const writers = 4
	scopes := []string{"inference:chat", "models:read"}
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writer := lab.NewClient(g.Base)
			for writeCtx.Err() == nil {
				r := writer.PostJSON(writeCtx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{
					"action": "set-scopes", "keyId": targets[i%len(targets)], "scopes": scopes})
				switch {
				case writeCtx.Err() != nil:
				case r.Err == nil && r.Status == 200:
					wrote.Add(1)
				default:
					failed.Add(1)
				}
			}
		}()
	}
	loaded, err := reads(window)
	stopWriters()
	wg.Wait()
	if err != nil {
		return err
	}
	if n := failed.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "bench: %d writes failed under contention\n", n)
	}
	q, l := lab.Sorted(quiet), lab.Sorted(loaded)
	b.add("read with no writers, p50", "ms", ms(lab.Percentile(q, 50)))
	b.add("read with no writers, p99", "ms", ms(lab.Percentile(q, 99)))
	b.add("read with 4 writers, p50", "ms", ms(lab.Percentile(l, 50)))
	b.add("read with 4 writers, p99", "ms", ms(lab.Percentile(l, 99)))
	b.add("failed writes with 4 writers", "count", float64(failed.Load()))
	b.metrics = append(b.metrics, metric{Name: "writes per second with 4 writers", Unit: "per s (higher is better)", Value: float64(wrote.Load()) / window.Seconds()})
	fmt.Fprintf(os.Stderr, "bench: %-42s %12.2f per s\n", "writes per second with 4 writers", float64(wrote.Load())/window.Seconds())
	return nil
}

func markdown(res result, compare string, regressions []string) string {
	var s strings.Builder
	verdict := ""
	if compare != "" {
		verdict = ": no regressions"
		if len(regressions) > 0 {
			verdict = ": REGRESSIONS"
		}
	}
	fmt.Fprintf(&s, "# Benchmark%s\n\n%s, commit `%s`, one-minute load %s. Fake backends answering at once, so figures are the gateway's own cost.\n\n", verdict, res.Platform, res.Commit, res.Load)
	fmt.Fprintln(&s, "| measure | value | budget |")
	fmt.Fprintln(&s, "|---|---:|---:|")
	for _, m := range res.Metrics {
		budget := "-"
		if m.Budget > 0 {
			budget = fmt.Sprintf("%.4g", m.Budget)
		}
		fmt.Fprintf(&s, "| %s | %.2f %s | %s |\n", m.Name, m.Value, m.Unit, budget)
	}
	if len(regressions) > 0 {
		fmt.Fprintf(&s, "\n## Regressions against %s\n\n", compare)
		for _, r := range regressions {
			fmt.Fprintf(&s, "- %s\n", r)
		}
	}
	return s.String()
}

func gitCommit() string { return commitOf(".") }

func loadAverage() string {
	var la [3]float64
	if n, err := getLoad(&la); err == nil && n > 0 {
		return fmt.Sprintf("%.1f", la[0])
	}
	return "unknown"
}
