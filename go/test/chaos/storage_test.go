package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

func dbPath(g *lab.Gateway) string { return filepath.Join(g.Root, "local-base.db") }

// noServerError fails on a 500-class answer that is not the documented 503, and
// on a connection error: whatever is broken, the gateway answers.
func noServerError(t *testing.T, what string, r lab.Call) {
	t.Helper()
	if r.Err != nil {
		t.Errorf("%s: connection error: %v", what, r.Err)
		return
	}
	if r.Status >= 500 && (r.Status != 503 || r.RetryAfter == "") {
		t.Errorf("%s: %d code %q Retry-After %q: %.300s", what, r.Status, r.Code, r.RetryAfter, r.Body)
	}
}

// Another process holds the database's exclusive lock (a CLI, a backup): health
// still answers at once, requests wait out the busy timeout and answer rather
// than hang, and everything works once the lock is gone.
func TestDatabaseLocked(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	st, err := store.Open(dbPath(g))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conn, err := st.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
				t.Errorf("releasing the lock: %v", err)
			}
		}
	}
	defer unlock()

	began := time.Now()
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health under a database lock: %d %.200s", r.Status, r.Body)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("health waited %v for a lock it does not need", took)
	}
	const clients = 4
	results := make(chan lab.Call, clients)
	for range clients {
		go func() { results <- c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello")) }()
	}
	for range clients {
		r := <-results
		noServerError(t, "a request under a database lock", r)
		if r.Dur > 12*time.Second {
			t.Errorf("a request took %v under a lock with a 5 s busy timeout", r.Dur)
		}
		if r.Status != 200 && r.Status != 503 {
			t.Errorf("a request under a database lock: %d %.300s", r.Status, r.Body)
		}
	}
	unlock()
	mustChat(t, ctx, c, g, lab.ChatSmall)
}

// A damaged database at startup: the gateway says so and exits, without a panic
// and without touching the file, so a backup or a repair can still save it.
func TestDatabaseCorruptAtStart(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	c.CloseIdle()
	if _, err := g.Stop(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	path := dbPath(g)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged := bytes.Clone(original)
	copy(damaged, bytes.Repeat([]byte{0xDB}, 4096))
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileLength(t, g.LogPath)
	began := time.Now()
	g2, err := workspace.Start(ctx, lab.Options{Name: g.Opts.Name, Reuse: g, CtxSize: 8192})
	if err == nil {
		g2.Stop(30 * time.Second)
		t.Fatal("the gateway started on a damaged database")
	}
	if took := time.Since(began); took > 20*time.Second {
		t.Errorf("it took %v to refuse a damaged database", took)
	}
	log := logSince(t, g.LogPath, before)
	if strings.Contains(log, "panic:") || strings.Contains(log, "goroutine ") {
		t.Errorf("a panic on a damaged database:\n%.1500s", log)
	}
	if !strings.Contains(strings.ToLower(log), "database") && !strings.Contains(strings.ToLower(log), "sqlite") {
		t.Errorf("the refusal does not mention the database:\n%.800s", log)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, damaged) {
		t.Error("the refusal changed the damaged file")
	}
	// Restored from a backup, it serves again with the same keys.
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	g3, err := workspace.Start(ctx, lab.Options{Name: g.Opts.Name, Reuse: g, CtxSize: 8192})
	if err != nil {
		t.Fatalf("after restoring the file: %v", err)
	}
	t.Cleanup(func() { g3.Stop(30 * time.Second); workspace.KillStray() })
	mustChat(t, ctx, lab.NewClient(g3.Base), g3, lab.ChatSmall)
}

func fileLength(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func logSince(t *testing.T, path string, offset int64) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[min(offset, int64(len(b))):])
}

// The database damaged under a running gateway: it keeps answering, with a
// documented refusal where it needs the database, and it stops cleanly.
func TestDatabaseDamagedWhileRunning(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	f, err := os.OpenFile(dbPath(g), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xDB}, 64<<10), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for i := range 5 {
		noServerError(t, fmt.Sprintf("chat %d on a damaged database", i), c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello")))
		noServerError(t, fmt.Sprintf("models %d on a damaged database", i), c.Do(ctx, "GET", "/v1/models", g.Keys.User, "", nil))
		noServerError(t, fmt.Sprintf("keys %d on a damaged database", i), c.Do(ctx, "GET", "/_localbase/api-keys", g.Keys.Admin, "", nil))
	}
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health on a damaged database: %d %.200s", r.Status, r.Body)
	}
	if _, err := g.Stop(30 * time.Second); err != nil {
		t.Errorf("stopping on a damaged database: %v", err)
	}
}

// --- runs and checkpoints across a kill --------------------------------------

type runRecord struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Output *struct {
		Content string `json:"content"`
	} `json:"output"`
}

// A gateway killed with a run in progress: on restart the run is interrupted
// with the output of its last checkpoint. Damaged checkpoints beside it cost
// nothing but themselves, and stored times from another clock stay harmless: a
// run retained past its expiry goes, and one dated far ahead can still be removed.
func TestRunsAfterKill(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	key := g.Keys.Runs[0]
	create := c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:slow 400"))
	if create.Status != 202 {
		t.Fatalf("create: %d %.300s", create.Status, create.Body)
	}
	var run runRecord
	json.Unmarshal(create.Body, &run)
	runsDir := filepath.Join(g.Root, "runs")
	checkpoint := filepath.Join(runsDir, run.ID+".json")
	// A checkpoint with output is on disk within its one-second interval.
	var saved map[string]any
	waitFor(t, "a checkpoint with output", 10*time.Second, func() bool {
		b, err := os.ReadFile(checkpoint)
		if err != nil || json.Unmarshal(b, &saved) != nil {
			return false
		}
		out, _ := saved["output"].(map[string]any)
		content, _ := out["content"].(string)
		return strings.Count(content, " ") >= 3
	})
	if err := g.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-g.Done()
	workspace.KillStray() // the orphan is TestGatewayKilledOutright's subject

	// Things a crash or a bad disk could leave beside it.
	plant := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runsDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant("garbage.json", []byte("not json"))
	plant("run_truncated.json", []byte(`{"id":"run_truncated","status":"in_prog`))
	plant("run_wrongid.json", []byte(`{"id":"run_other","status":"completed","owner":"x"}`))
	plant("run_big.json", bytes.Repeat([]byte(" "), 8<<20))
	plant("notes.txt", []byte("not a checkpoint"))
	dated := func(id, expires string) {
		t.Helper()
		rec := map[string]any{}
		for k, v := range saved {
			rec[k] = v
		}
		rec["id"], rec["status"], rec["expiresAt"], rec["endedAt"] = id, "cancelled", expires, "2000-01-01T00:00:00Z"
		b, _ := json.Marshal(rec)
		plant(id+".json", b)
	}
	dated("run_expired", "2000-01-01T00:00:00Z")
	dated("run_farahead", "2199-01-01T00:00:00Z")

	g2, err := workspace.Start(ctx, lab.Options{Name: g.Opts.Name, Reuse: g, CtxSize: 8192})
	if err != nil {
		t.Fatalf("restart among damaged checkpoints: %v", err)
	}
	t.Cleanup(func() { g2.Stop(30 * time.Second); workspace.KillStray() })
	c2 := lab.NewClient(g2.Base)

	view := c2.Do(ctx, "GET", "/_localbase/runs/"+run.ID, key, "", nil)
	var got runRecord
	if view.Status != 200 || json.Unmarshal(view.Body, &got) != nil {
		t.Fatalf("the interrupted run: %d %.300s", view.Status, view.Body)
	}
	if got.Status != "interrupted" || got.Reason != "gateway_restart" {
		t.Errorf("a run across a kill: status %q reason %q", got.Status, got.Reason)
	}
	if got.Output == nil || !strings.HasPrefix(got.Output.Content, "w1 w2 w3 ") {
		t.Errorf("the partial output was lost: %+v", got.Output)
	}
	if events := c2.Do(ctx, "GET", "/_localbase/runs/"+run.ID+"/events", key, "", nil); !strings.Contains(string(events.Body), "event: end") {
		t.Errorf("a restored run's events have no end: %d %.300s", events.Status, events.Body)
	}
	for _, name := range []string{"garbage.json", "run_truncated.json", "run_wrongid.json", "run_big.json", "notes.txt", "run_expired.json"} {
		if _, err := os.Stat(filepath.Join(runsDir, name)); err == nil {
			t.Errorf("%s was left in runs/", name)
		}
	}
	if r := c2.Do(ctx, "GET", "/_localbase/runs/run_expired", key, "", nil); r.Status != 404 {
		t.Errorf("a run past its expiry: %d %.200s", r.Status, r.Body)
	}
	if r := c2.Do(ctx, "GET", "/_localbase/runs", key, "", nil); r.Status != 200 {
		t.Errorf("the run list: %d %.200s", r.Status, r.Body)
	}
	if r := c2.Do(ctx, "DELETE", "/_localbase/runs/run_farahead", key, "", nil); r.Status != 200 {
		t.Errorf("removing a run dated far ahead: %d %.200s", r.Status, r.Body)
	}
	if r := c2.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:reply hi")); r.Status != 202 {
		t.Errorf("a new run after all that: %d %.300s", r.Status, r.Body)
	}
}

// Keys against the stored clock: one past its expiry stops working at once, with
// nothing cached, and one dated far ahead works.
func TestKeyExpiryFollowsStoredTime(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	st, err := store.Open(dbPath(g))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := g.Keys.Runs[1]
	setExpiry := func(when string) {
		t.Helper()
		prefix := key[:min(len(key), 12)]
		res, err := st.DB.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE key_hash = ?`, when, store.HashKey(key))
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("updated %d rows for %s...", n, prefix)
		}
	}
	models := func() int { return c.Do(ctx, "GET", "/v1/models", key, "", nil).Status }
	if s := models(); s != 200 {
		t.Fatalf("a good key: %d", s)
	}
	setExpiry("2000-01-01T00:00:00.000Z")
	if s := models(); s != 401 {
		t.Errorf("a key past its expiry: %d", s)
	}
	setExpiry("2199-01-01T00:00:00.000Z")
	if s := models(); s != 200 {
		t.Errorf("a key dated far ahead: %d", s)
	}
}
