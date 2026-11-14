package system

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// plantUsage writes rows the way a gateway would, then lets go of the database.
func plantUsage(t *testing.T, rootDir string, events ...usage.Event) {
	t.Helper()
	st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := usage.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		rec.Add(e)
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func usageJSON(t *testing.T, rootDir string, args ...string) usage.Report {
	t.Helper()
	res := runCLI(t, rootDir, append([]string{"--json", "usage"}, args...)...)
	if res.code != 0 {
		t.Fatalf("usage %v: %d %s", args, res.code, res.stderr)
	}
	var env struct {
		Data usage.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &env); err != nil {
		t.Fatalf("usage %v: %v\n%s", args, err, res.stdout)
	}
	return env.Data
}

func newUsageRoot(t *testing.T) string {
	t.Helper()
	rootDir := filepath.Join(fsguard.ScratchDir(t), "root")
	if res := runCLI(t, rootDir, "--json", "init"); res.code != 0 {
		t.Fatalf("init: %s", res.stderr)
	}
	return rootDir
}

// `local-base usage` with no gateway: it reads and writes the database (DESIGN §32.7).
func TestCLIUsage(t *testing.T) {
	rootDir := newUsageRoot(t)
	empty := runCLI(t, rootDir, "usage")
	if empty.code != 0 || !strings.Contains(empty.stdout, "No requests") || !strings.Contains(empty.stdout, "never prompts or replies") {
		t.Errorf("empty: %d\n%s\n%s", empty.code, empty.stdout, empty.stderr)
	}
	now := time.Now()
	plantUsage(t, rootDir,
		usage.Event{Kind: "api-key", Principal: "key_a", Route: "chat", Model: "m1", Status: 200, Duration: time.Second, InputTokens: 10, OutputTokens: 5, HasTokens: true, At: now},
		usage.Event{Kind: "api-key", Principal: "key_a", Route: "chat", Model: "m1", Status: 500, Duration: time.Second, At: now},
		usage.Event{Kind: "api-key", Principal: "key_b", Route: "embeddings", Model: "m2", Status: 499, Duration: time.Second, At: now})

	got := usageJSON(t, rootDir, "--by", "key", "--by", "model")
	if got.Totals.Requests != 3 || got.Totals.Failed != 1 || got.Totals.Cancelled != 1 || got.Scope != "all" || len(got.Groups) != 2 {
		t.Errorf("%+v", got)
	}
	if one := usageJSON(t, rootDir, "--key", "key_b"); one.Totals.Requests != 1 {
		t.Errorf("--key: %+v", one.Totals)
	}
	if one := usageJSON(t, rootDir, "--route", "chat"); one.Totals.Requests != 2 {
		t.Errorf("--route: %+v", one.Totals)
	}
	table := runCLI(t, rootDir, "usage", "--by", "key")
	for _, want := range []string{"3 requests", "key_a", "WHO", "Recording is on", "kept for 30 days"} {
		if !strings.Contains(table.stdout, want) {
			t.Errorf("missing %q:\n%s", want, table.stdout)
		}
	}
	for name, args := range map[string][]string{
		"a bad range":     {"usage", "--range", "1y"},
		"a bad group":     {"usage", "--by", "person"},
		"a bad route":     {"usage", "--route", "nope"},
		"no confirmation": {"usage", "clear"},
		"a bad date":      {"usage", "clear", "--yes", "--before", "yesterday"},
		"too few days":    {"usage", "retention", "0"},
		"too many days":   {"usage", "retention", "366"},
		"not a number":    {"usage", "retention", "a"},
		"not on or off":   {"usage", "recording", "maybe"},
	} {
		if res := runCLI(t, rootDir, args...); res.code != 2 {
			t.Errorf("%s: exit %d: %s", name, res.code, res.stderr)
		}
	}
	if keep := usageJSON(t, rootDir); keep.Totals.Requests != 3 {
		t.Errorf("clear without --yes removed something: %+v", keep.Totals)
	}
	for _, args := range [][]string{{"usage", "retention", "7"}, {"usage", "recording", "off"}} {
		res := runCLI(t, rootDir, append([]string{"--json"}, args...)...)
		if res.code != 0 || !strings.Contains(res.stdout, `"via":"database"`) {
			t.Errorf("%v: %d %s %s", args, res.code, res.stdout, res.stderr)
		}
	}
	if got := usageJSON(t, rootDir); got.RetentionDays != 7 || got.Recording {
		t.Errorf("settings: %+v", got)
	}
	if res := runCLI(t, rootDir, "--json", "usage", "clear", "--yes"); res.code != 0 {
		t.Errorf("clear: %d %s", res.code, res.stderr)
	}
	if got := usageJSON(t, rootDir); got.Totals.Requests != 0 {
		t.Errorf("after clearing: %+v", got.Totals)
	}
}

// A browser identity is its owner hash, here too (DESIGN §32.1). The session row holds the
// email, so nothing may join to it.
func TestCLIUsageNoIdentity(t *testing.T) {
	rootDir := newUsageRoot(t)
	const email = "planted.person@example.test"
	owner := "browser:0123456789abcdef0123456789abcdef"
	st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateSession(context.Background(), st, owner, auth.Identity{Issuer: "i", Subject: "planted-subject", VerifiedEmail: email}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	plantUsage(t, rootDir, usage.Event{Kind: "browser", Principal: owner, Route: "chat", Status: 200, At: time.Now()})
	for _, args := range [][]string{{"usage"}, {"usage", "--by", "key"}, {"--json", "usage", "--by", "key"}, {"--json", "usage", "--by", "key", "--by", "model", "--by", "route"}} {
		res := runCLI(t, rootDir, args...)
		if res.code != 0 {
			t.Fatalf("%v: %d %s", args, res.code, res.stderr)
		}
		for _, planted := range []string{email, "planted-subject"} {
			if strings.Contains(res.stdout+res.stderr, planted) {
				t.Errorf("%v shows %q:\n%s", args, planted, res.stdout)
			}
		}
	}
	if got := usageJSON(t, rootDir, "--by", "key"); len(got.Groups) != 1 || got.Groups[0].Principal.ID != owner || got.Groups[0].Principal.Name != "" {
		t.Errorf("%+v", got.Groups)
	}
}

// writeOwner is a gateway lock that says who holds this root.
func writeOwner(t *testing.T, rootDir string, pid int, host string, port int) {
	t.Helper()
	dir := filepath.Join(rootDir, "runtime", "gateway.lock")
	if err := fsguard.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	canon, err := root.Canonical(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"version": 2, "pid": pid, "root": canon, "host": host, "port": port, "instanceToken": "planted-token"})
	if err := fsguard.WriteFile(filepath.Join(dir, "owner.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// With a gateway on the root, nothing writes behind its back (DESIGN §32.4).
func TestCLIUsageRefusesWithoutTheGateway(t *testing.T) {
	writes := [][]string{{"--json", "usage", "clear", "--yes"}, {"--json", "usage", "retention", "7"}, {"--json", "usage", "recording", "off"}}
	settled := func(t *testing.T, rootDir string) {
		t.Helper()
		// Read with the lock out of the way: the point is what is on disk.
		st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var rows int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM usage_hourly`).Scan(&rows); err != nil || rows != 1 {
			t.Errorf("rows %d, %v: something was cleared", rows, err)
		}
		if _, set, _ := st.Setting(context.Background(), "usage.retention-days"); set {
			t.Error("retention was changed")
		}
		if _, set, _ := st.Setting(context.Background(), "usage.recording"); set {
			t.Error("recording was changed")
		}
	}
	event := usage.Event{Kind: "api-key", Principal: "key_a", Route: "chat", Status: 200, At: time.Now()}

	t.Run("the gateway does not answer", func(t *testing.T) {
		rootDir := newUsageRoot(t)
		plantUsage(t, rootDir, event)
		ln := httptest.NewUnstartedServer(nil)
		port := ln.Listener.Addr().(interface{ String() string }).String()
		ln.Listener.Close()
		_, p, _ := strings.Cut(port, ":")
		n, _ := strconv.Atoi(p)
		writeOwner(t, rootDir, os.Getpid(), "127.0.0.1", n)
		for _, w := range writes {
			res := runCLI(t, rootDir, w...)
			if res.code != 1 || !strings.Contains(res.stdout+res.stderr, "Nothing changed") {
				t.Errorf("%v: %d %s %s", w, res.code, res.stdout, res.stderr)
			}
		}
		settled(t, rootDir)
	})
	t.Run("the gateway has no such route", func(t *testing.T) {
		rootDir := newUsageRoot(t)
		plantUsage(t, rootDir, event)
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		u, _ := url.Parse(srv.URL)
		n, _ := strconv.Atoi(u.Port())
		writeOwner(t, rootDir, os.Getpid(), "127.0.0.1", n)
		for _, w := range writes {
			res := runCLI(t, rootDir, w...)
			if res.code != 1 || !strings.Contains(res.stdout+res.stderr, "Nothing changed") {
				t.Errorf("%v: %d %s %s", w, res.code, res.stdout, res.stderr)
			}
		}
		settled(t, rootDir)
	})
	t.Run("the lock cannot be read", func(t *testing.T) {
		rootDir := newUsageRoot(t)
		plantUsage(t, rootDir, event)
		dir := filepath.Join(rootDir, "runtime", "gateway.lock")
		if err := fsguard.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := fsguard.WriteFile(filepath.Join(dir, "owner.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, w := range writes {
			res := runCLI(t, rootDir, w...)
			if res.code != 1 || !strings.Contains(res.stdout+res.stderr, "Nothing changed") {
				t.Errorf("%v: %d %s %s", w, res.code, res.stdout, res.stderr)
			}
		}
		settled(t, rootDir)
	})
	t.Run("a lock left by a dead gateway is no gateway", func(t *testing.T) {
		rootDir := newUsageRoot(t)
		plantUsage(t, rootDir, event)
		dead := exec.Command("true")
		if err := dead.Run(); err != nil {
			t.Fatal(err)
		}
		writeOwner(t, rootDir, dead.Process.Pid, "127.0.0.1", 1)
		res := runCLI(t, rootDir, "--json", "usage", "clear", "--yes")
		if res.code != 0 || !strings.Contains(res.stdout, `"via":"database"`) {
			t.Errorf("%d %s %s", res.code, res.stdout, res.stderr)
		}
	})
	// Reading never needs the gateway.
	t.Run("reading still works", func(t *testing.T) {
		rootDir := newUsageRoot(t)
		plantUsage(t, rootDir, event)
		writeOwner(t, rootDir, os.Getpid(), "127.0.0.1", 1)
		if got := usageJSON(t, rootDir); got.Totals.Requests != 1 {
			t.Errorf("%+v", got.Totals)
		}
	})
}

// A real gateway: the CLI reaches it, so what it holds in memory goes too. A refused
// request is enough to count, so no backend runs.
func TestCLIUsageGoesThroughGateway(t *testing.T) {
	var target *harness.Target
	for _, c := range harness.Targets(t) {
		if c.Name == "go" {
			target = c
		}
	}
	if target == nil || target.Root == "" {
		t.Skip("no Go gateway launched")
	}
	api := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, target.BaseURL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+target.Admin)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	refused := func() {
		t.Helper()
		if status, b := api("POST", "/v1/chat/completions", `{"model":""}`); status != 400 {
			t.Fatalf("a refused request: %d %s", status, b)
		}
	}
	current := func() usage.Report {
		t.Helper()
		status, b := api("GET", "/_localbase/usage?scope=all", "")
		if status != 200 {
			t.Fatalf("usage: %d %s", status, b)
		}
		var rep usage.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	refused()
	if !waitUntil(5*time.Second, func() bool { return current().Totals.Requests > 0 }) {
		t.Fatal("the request was never counted")
	}
	// In memory only, right now: a direct database write could not reach it.
	res := runCLI(t, target.Root, "--json", "usage", "clear", "--yes")
	if res.code != 0 || !strings.Contains(res.stdout, `"via":"gateway"`) {
		t.Fatalf("clear: %d %s %s", res.code, res.stdout, res.stderr)
	}
	if got := current().Totals.Requests; got != 0 {
		t.Errorf("after clearing through the gateway: %d, want the counts in memory gone too", got)
	}
	res = runCLI(t, target.Root, "--json", "usage", "retention", "7")
	if res.code != 0 || !strings.Contains(res.stdout, `"via":"gateway"`) {
		t.Fatalf("retention: %d %s %s", res.code, res.stdout, res.stderr)
	}
	if got := current().RetentionDays; got != 7 {
		t.Errorf("retention, as the gateway sees it: %d", got)
	}
	res = runCLI(t, target.Root, "--json", "usage", "recording", "off")
	if res.code != 0 || !strings.Contains(res.stdout, `"via":"gateway"`) {
		t.Fatalf("recording: %d %s %s", res.code, res.stdout, res.stderr)
	}
	refused()
	time.Sleep(50 * time.Millisecond)
	if got := current(); got.Recording || got.Totals.Requests != 0 {
		t.Errorf("with recording off: %+v", got)
	}
	// Leave the shared gateway as it was.
	for _, args := range [][]string{{"--json", "usage", "recording", "on"}, {"--json", "usage", "retention", "30"}} {
		if res := runCLI(t, target.Root, args...); res.code != 0 {
			t.Errorf("%v: %d %s", args, res.code, res.stderr)
		}
	}
}

func waitUntil(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// The bundle never holds this table (DESIGN §32.1).
func TestUsageOutOfDiagnostics(t *testing.T) {
	rootDir := newUsageRoot(t)
	const principal, model = "key_PLANTEDUSAGEPRINCIPAL", "PLANTEDUSAGEMODEL"
	plantUsage(t, rootDir, usage.Event{Kind: "api-key", Principal: principal, Route: "chat", Model: model, Status: 200, At: time.Now()})
	out := filepath.Join(fsguard.ScratchDir(t), "bundle.zip")
	if res := runCLI(t, rootDir, "diagnostics", "--output", out); res.code != 0 {
		t.Fatalf("diagnostics: %d %s", res.code, res.stderr)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) == 0 {
		t.Fatal("an empty bundle")
	}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		for _, planted := range []string{principal, model, "usage_hourly"} {
			if strings.Contains(string(b), planted) {
				t.Errorf("%s holds %q", f.Name, planted)
			}
		}
	}
}
