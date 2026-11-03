package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// What each route is, for the audit log. A new route fails TestEveryRouteIsClassifiedForAudit
// until it is here.
var routeAuditClasses = map[string]string{
	"/_localbase/computer-name":        "limited",
	"/_localbase/hf-token":             "limited",
	"/_localbase/model-backend":        "limited",
	"/_localbase/model-management":     "limited",
	"/_localbase/default-chat-models":  "limited",
	"/_localbase/model-discovery":      "limited",
	"/_localbase/model-imports":        "limited",
	"/_localbase/voices":               "limited",
	"/_localbase/voices/":              "limited",
	"/_localbase/models/{model_id...}": "limited: unload and placement; the rest only read",

	"/_localbase/access-management": "unconditional",
	"/_localbase/api-keys":          "unconditional",
	"/_localbase/local-sessions":    "unconditional",
	"/_localbase/usage":             "unconditional",
	"/_localbase/update":            "unconditional",

	"/_localbase/runs":            "deliberately not: a person's own work, not administration",
	"/_localbase/runs/{rest...}":  "deliberately not: a person's own work, not administration",
	"/v1/videos":                  "deliberately not: a person's own work, not administration",
	"/v1/videos/{job_id}":         "deliberately not: a person's own work, not administration",
	"/v1/videos/{job_id}/content": "deliberately not: a person's own work, not administration",
	"/v1/videos/{job_id}/cancel":  "deliberately not: a person's own work, not administration",

	"/health": "read", "/health/ready": "read", "/openapi.json": "read",
	"/_localbase/instance": "read", "/_localbase/host": "read", "/_localbase/default-model": "read",
	"/_localbase/placement-state": "read", "/_localbase/backends": "read", "/_localbase/auth-profiles": "read",
	"/_localbase/models": "read", "/v1/models": "read", "/v1/models/{model...}": "read",

	"/v1/chat/completions": "inference", "/v1/embeddings": "inference", "/v1/systemone": "inference",
	"/v1/audio/transcriptions": "inference", "/v1/audio/translations": "inference", "/v1/audio/speech": "inference",
	"/v1/images/generations": "inference", "/v1/images/edits": "inference",

	"/app": "pages", "/app/": "pages", "/{$}": "pages", "/": "nothing: unmatched paths",
}

func TestEveryRouteIsClassifiedForAudit(t *testing.T) {
	seen := map[string]bool{}
	for _, pattern := range gatewayPatterns(t) {
		seen[pattern] = true
		if _, ok := routeAuditClasses[pattern]; !ok && !isSignInPath(pattern) {
			t.Errorf("%s isn't in routeAuditClasses: say whether it changes anything, and audit it if it does", pattern)
		}
	}
	for pattern := range routeAuditClasses {
		if !seen[pattern] {
			t.Errorf("routeAuditClasses names %s, which isn't a route", pattern)
		}
	}
}

type auditFixture struct {
	t         *testing.T
	base      string
	srv       *Server
	logs      *lockedBuffer
	key, id   string
	noScopes  string
	noScopeID string
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()
	logs := &lockedBuffer{}
	testLog = logs
	ts := openAPIGateway(t)
	testLog = nil
	srv := runServer(ts)
	ctx := context.Background()
	k, key, err := srv.Store.CreateAPIKey(ctx, "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	none, noScopes, err := srv.Store.CreateAPIKey(ctx, "none", []string{})
	if err != nil {
		t.Fatal(err)
	}
	return &auditFixture{t: t, base: ts.URL, srv: srv, logs: logs, key: key, id: k.ID, noScopes: noScopes, noScopeID: none.ID}
}

func (f *auditFixture) send(key, method, path, contentType string, body []byte) (int, []byte) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.base+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// auditEvents reads the lines since a point in the log that are audit lines.
func (f *auditFixture) auditEvents(from int) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(f.logs.String()[from:], "\n") {
		if !strings.Contains(line, `"event":"audit.`) {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			f.t.Fatalf("%q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// Each real change on each limited route writes one line, naming the event, the actor and the
// target; each refused change writes one too; what only looks writes none.
func TestEachRouteWritesItsAuditLine(t *testing.T) {
	f := newAuditFixture(t)
	f.srv.changeAudit.sampler.Burst = 1000
	f.srv.refusedAudit.Burst = 1000
	// A loaded model to unload.
	if status, out := f.send(f.key, "POST", "/v1/chat/completions", "application/json", []byte(chatBody("hi"))); status != 200 {
		t.Fatalf("load: %d %s", status, out)
	}
	source := filepath.Join(scratchDir(t), "audit-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, source, 1<<20)
	const planted = "hf_PLANTEDtokenValue0123456789abcdef"
	var voice, job string
	var clip bytes.Buffer
	mw := multipart.NewWriter(&clip)
	_ = mw.WriteField("name", "Audit voice")
	_ = mw.WriteField("consent", "true")
	part, _ := mw.CreateFormFile("audio", "a.wav")
	_, _ = part.Write(voiceClipWAV())
	_ = mw.Close()
	js := func(s string) []byte { return []byte(s) }
	const jsonType = "application/json"

	for _, c := range []struct {
		name, method, path, contentType string
		body                            func() []byte
		status                          int
		event, action, targetKind       string
		target                          func() string
		after                           func(out []byte)
	}{
		{"computer name", "PUT", "/_localbase/computer-name", jsonType, func() []byte { return js(`{"name":"PLANTED computer"}`) }, 200, "computer-name", "set", "computer-name", nil, nil},
		{"token saved", "POST", "/_localbase/hf-token", jsonType, func() []byte { return js(`{"action":"save","token":"` + planted + `","validate":false}`) }, 200, "hf-token", "save", "hf-token", nil, nil},
		{"token cleared", "POST", "/_localbase/hf-token", jsonType, func() []byte { return js(`{"action":"clear"}`) }, 200, "hf-token", "clear", "hf-token", nil, nil},
		{"backend reset", "POST", "/_localbase/model-backend", jsonType, func() []byte { return js(`{"action":"reset","model":"` + chatModel + `"}`) }, 200, "backend", "reset", "model", func() string { return chatModel }, nil},
		{"default models", "POST", "/_localbase/default-chat-models", jsonType, func() []byte { return js(`{"models":["` + chatModel + `"]}`) }, 200, "default-models", "set", "model", func() string { return chatModel }, nil},
		{"model enabled", "POST", "/_localbase/model-management", jsonType, func() []byte { return js(`{"modelId":"` + chatModel + `","action":"enable"}`) }, 200, "models", "enable", "model", func() string { return chatModel }, nil},
		{"placement set", "PUT", "/_localbase/models/" + chatModel + "/placement", jsonType, func() []byte { return js(`{"mode":"auto"}`) }, 200, "placement", "set", "model", func() string { return chatModel }, nil},
		{"placement cleared", "DELETE", "/_localbase/models/" + chatModel + "/placement", "", func() []byte { return nil }, 200, "placement", "clear", "model", func() string { return chatModel }, nil},
		{"unload", "POST", "/_localbase/models/" + chatModel + "/unload", "", func() []byte { return nil }, 200, "unload", "unload", "model", func() string { return chatModel }, nil},
		{"import started", "POST", "/_localbase/model-imports", jsonType, func() []byte {
			b, _ := json.Marshal(map[string]any{"action": "start", "source": source, "task": "chat", "id": "audit-import", "name": "Audit"})
			return b
		}, 202, "imports", "start", "import-job", func() string { return job }, nil},
		{"voice created", "POST", "/_localbase/voices", mw.FormDataContentType(), clip.Bytes, 201, "voices", "create", "voice", func() string { return voice }, nil},
		{"voice renamed", "PATCH", "/_localbase/voices/", jsonType, func() []byte { return js(`{"name":"PLANTED name"}`) }, 200, "voices", "rename", "voice", func() string { return voice }, nil},
		{"voice deleted", "DELETE", "/_localbase/voices/", "", func() []byte { return nil }, 200, "voices", "delete", "voice", func() string { return voice }, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.path
			if strings.HasSuffix(path, "/voices/") {
				path += voice
			}
			from := len(f.logs.String())
			status, out := f.send(f.key, c.method, path, c.contentType, c.body())
			if status != c.status && !(c.name == "voice deleted" && status == 204) {
				t.Fatalf("%d %s", status, out)
			}
			if c.name == "voice created" {
				var v struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(out, &v)
				voice = v.ID
			}
			if c.name == "import started" {
				var v struct {
					Job string `json:"job"`
				}
				_ = json.Unmarshal(out, &v)
				job = v.Job
				if !waitFor(openAPIWait, func() bool {
					_, b := f.send(f.key, "GET", "/_localbase/model-imports?id="+job, "", nil)
					return !bytes.Contains(b, []byte(`"state":"running"`))
				}) {
					t.Fatal("the import did not finish")
				}
			}
			events := f.auditEvents(from)
			if len(events) != 1 {
				t.Fatalf("%d audit lines: %v", len(events), events)
			}
			ev := events[0]
			if ev["event"] != "audit."+c.event+"-changed" || ev["action"] != c.action || ev["actor_kind"] != "api-key" || ev["actor_id"] != f.id || ev["target_kind"] != c.targetKind {
				t.Errorf("%v", ev)
			}
			want := ""
			if c.target != nil {
				want = c.target()
			}
			if ev["target_id"] != want || (c.target != nil && want == "") {
				t.Errorf("target %v, want %q", ev["target_id"], want)
			}
		})
	}

	t.Run("import cancelled", func(t *testing.T) {
		if err := f.srv.Store.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
			t.Fatal(err)
		}
		// It holds the download open, so the cancel lands on a job that is running.
		hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write(make([]byte, 16<<10)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if r.Header.Get("Range") == "bytes=0-0" {
				return
			}
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
		}))
		defer hold.Close()
		b, _ := json.Marshal(map[string]any{"action": "start", "source": hold.URL + "/m.gguf", "allowHttp": true, "confirmCustomSource": true, "task": "chat", "id": "audit-held"})
		status, out := f.send(f.key, "POST", "/_localbase/model-imports", jsonType, b)
		if status != 202 {
			t.Fatalf("%d %s", status, out)
		}
		var started struct{ Job string }
		_ = json.Unmarshal(out, &started)
		from := len(f.logs.String())
		b, _ = json.Marshal(map[string]any{"action": "cancel", "job": started.Job})
		status, out = f.send(f.key, "POST", "/_localbase/model-imports", jsonType, b)
		if status != 200 {
			t.Fatalf("%d %s", status, out)
		}
		events := f.auditEvents(from)
		if len(events) != 1 || events[0]["action"] != "cancel" || events[0]["target_id"] != started.Job || events[0]["target_kind"] != "import-job" {
			t.Errorf("%v", events)
		}
		if !waitFor(10*time.Second, func() bool {
			_, b := f.send(f.key, "GET", "/_localbase/model-imports?id="+started.Job, "", nil)
			return !bytes.Contains(b, []byte(`"state":"running"`))
		}) {
			t.Error("the job did not end")
		}
	})

	t.Run("cancel with the instance token is the command line", func(t *testing.T) {
		release, err := f.srv.Root.Lock("127.0.0.1", 1)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		f.srv.mgmt.mu.Lock()
		f.srv.mgmt.operations[chatModel] = &mgmtOperation{Action: "install", State: "cancelled", Detail: cancelDetail}
		f.srv.mgmt.mu.Unlock()
		from := len(f.logs.String())
		req, _ := http.NewRequest("POST", f.base+"/_localbase/model-management", strings.NewReader(`{"modelId":"`+chatModel+`","action":"cancel"}`))
		req.Header.Set("X-Localbase-Instance-Token", f.srv.Root.Instance().InstanceToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		events := f.auditEvents(from)
		if resp.StatusCode != 200 || len(events) != 1 || events[0]["actor_kind"] != "cli" || events[0]["action"] != "cancel" {
			t.Errorf("%d %v", resp.StatusCode, events)
		}
	})

	// Refused, by a key that holds nothing: a line, with no target a caller made up.
	for _, c := range []struct{ area, method, path, body, target string }{
		{"computer-name", "PUT", "/_localbase/computer-name", `{"name":"x"}`, ""},
		{"hf-token", "POST", "/_localbase/hf-token", `{"action":"save","token":"` + planted + `"}`, ""},
		{"backend", "POST", "/_localbase/model-backend", `{"action":"set","model":"PLANTED-made-up","backendId":"x"}`, ""},
		{"default-models", "POST", "/_localbase/default-chat-models", `{"models":["PLANTED-made-up"]}`, ""},
		{"models", "POST", "/_localbase/model-management", `{"modelId":"PLANTED-made-up","action":"install"}`, ""},
		{"models", "POST", "/_localbase/model-management", `{"modelId":"x\n{\"event\":\"audit.changes-summary\",\"count\":0}","action":"install"}`, ""},
		{"models", "POST", "/_localbase/model-management", `{"modelId":"` + chatModel + `","action":"install"}`, chatModel},
		{"placement", "PUT", "/_localbase/models/PLANTED-made-up/placement", `{"mode":"auto"}`, ""},
		{"unload", "POST", "/_localbase/models/PLANTED-made-up/unload", "", ""},
		{"imports", "POST", "/_localbase/model-imports", `{"action":"cancel","job":"PLANTED-made-up"}`, ""},
		{"discovery", "POST", "/_localbase/model-discovery", `{"action":"install-recipe","modelId":"PLANTED-made-up"}`, ""},
		{"voices", "DELETE", "/_localbase/voices/voice-0123456789ab", "", ""},
	} {
		t.Run("refused "+c.area, func(t *testing.T) {
			from := len(f.logs.String())
			status, out := f.send(f.noScopes, c.method, c.path, jsonType, js(c.body))
			if status != 403 {
				t.Fatalf("%d %s", status, out)
			}
			events := f.auditEvents(from)
			if len(events) != 1 || events[0]["event"] != "audit."+c.area+"-refused" || events[0]["actor_id"] != f.noScopeID || events[0]["target_id"] != c.target {
				t.Errorf("%v", events)
			}
		})
	}

	// What only looks, or asks for a method the route doesn't have, writes nothing.
	t.Run("not changes", func(t *testing.T) {
		from := len(f.logs.String())
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/_localbase/hf-token", ""},
			{"POST", "/_localbase/model-imports", `{"action":"resolve","source":"` + source + `"}`},
			{"POST", "/_localbase/model-discovery", `{"action":"search","query":"x"}`},
			{"GET", "/_localbase/models/" + chatModel + "/placement", ""},
			{"DELETE", "/_localbase/hf-token", ""},
		} {
			f.send(f.key, c.method, c.path, jsonType, js(c.body))
		}
		if events := f.auditEvents(from); len(events) != 0 {
			t.Errorf("%v", events)
		}
	})

	// Last, because it replaces the catalog the rest of this uses.
	// A recipe install: its refusal comes back in a 200, and the request is what's recorded.
	t.Run("install recipe", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "catalog", "feed-v2.json"))
		if err != nil {
			t.Fatal(err)
		}
		feed, err := catalog.ParseFeed(raw)
		if err != nil {
			t.Fatal(err)
		}
		annotations, _ := catalog.ParseAnnotations(feed.AnnotationItems)
		layered, err := catalog.LoadLayers("", &catalog.RemoteLayer{URL: "feed", Annotations: annotations})
		if err != nil {
			t.Fatal(err)
		}
		f.srv.Catalog.Replace(layered)
		from := len(f.logs.String())
		status, out := f.send(f.key, "POST", "/_localbase/model-discovery", jsonType, js(`{"action":"install-recipe","modelId":"`+decisionModel+`"}`))
		if status != 200 {
			t.Fatalf("%d %s", status, out)
		}
		events := f.auditEvents(from)
		if len(events) != 1 || events[0]["event"] != "audit.discovery-changed" || events[0]["action"] != "install-recipe" || events[0]["target_id"] != decisionModel {
			t.Errorf("%v", events)
		}
	})

	if logged := f.logs.String(); strings.Contains(logged, planted) || strings.Contains(logged, "PLANTED") {
		i := strings.Index(logged, "PLANTED")
		t.Errorf("a value from a request is in the log: %q", logged[max(0, i-120):min(len(logged), i+60)])
	}
}

// The limit is one budget, and what it holds back is said, by area, once a minute and at the end.
func TestChangeLimiter(t *testing.T) {
	l := newChangeLimiter()
	now := time.Unix(1000, 0)
	allowed := 0
	for i := 0; i < 50; i++ {
		area := "placement"
		if i%2 == 0 {
			area = "unload"
		}
		if l.allow(now, area) {
			allowed++
		}
	}
	if allowed != changeAuditBurst {
		t.Errorf("%d allowed, want %d", allowed, changeAuditBurst)
	}
	if n, _ := l.flush(now); n != 0 {
		t.Errorf("said %d before the minute ended", n)
	}
	later := now.Add(changeAuditEvery)
	if !l.allow(later, "placement") {
		t.Error("the next minute's budget")
	}
	if n, areas := l.flush(later); n != 30 || areas != "placement=15,unload=15" {
		t.Errorf("%d %q", n, areas)
	}
	if n, _ := l.flush(later); n != 0 {
		t.Errorf("said twice: %d", n)
	}
	for i := 0; i < 25; i++ {
		l.allow(later, "voices")
	}
	if n, areas := l.drain(later); n != 6 || areas != "voices=6" {
		t.Errorf("drain: %d %q", n, areas)
	}
}

// Many distinct ids a caller chose grow nothing: the limiter is counted by area, and an id is
// never a key.
func TestChangeLimiterDoesNotGrowWithCallerChosenIDs(t *testing.T) {
	f := newAuditFixture(t)
	const n = 20_000
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		id := "caller-chosen-" + strconv.Itoa(i)
		switch i % 3 {
		case 0: // limited changes, each with its own text
			if status, _ := f.send(f.key, "PUT", "/_localbase/computer-name", "application/json", []byte(`{"name":"`+id+`"}`)); status != 200 {
				t.Fatal(status)
			}
		case 1: // refused, for a model nobody has
			f.send(f.noScopes, "POST", "/_localbase/model-management", "application/json", []byte(`{"modelId":"`+id+`","action":"install"}`))
		default: // a model id in the path
			f.send(f.key, "POST", "/_localbase/models/"+id+"/unload", "", nil)
		}
	}
	f.srv.changeAudit.mu.Lock()
	entries := len(f.srv.changeAudit.held)
	f.srv.changeAudit.mu.Unlock()
	if entries > 1 {
		t.Errorf("%d entries after %d distinct ids: %v", entries, n, f.srv.changeAudit.held)
	}
	http.DefaultClient.CloseIdleConnections()
	f.logs.mu.Lock()
	f.logs.b.Reset()
	f.logs.mu.Unlock()
	runtime.GC()
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d distinct ids: %d limiter entries, heap grew %d bytes", n, entries, grew)
	if grew > 32<<20 {
		t.Errorf("the heap grew %d bytes", grew)
	}
}

// The file is what a person reads, so a change made while the limit is in force keeps its line,
// for the two areas that are never limited.
func TestAFloodDoesNotHideKeyChanges(t *testing.T) {
	f := newAuditFixture(t)
	for i := 0; i < 100; i++ {
		if status, out := f.send(f.key, "PUT", "/_localbase/computer-name", "application/json", []byte(`{"name":"x"}`)); status != 200 {
			t.Fatalf("%d %s", status, out)
		}
	}
	if status, out := f.send(f.key, "POST", "/_localbase/api-keys", "application/json", []byte(`{"action":"create","name":"later","scopes":["keys:read"]}`)); status != 200 && status != 201 {
		t.Fatalf("%d %s", status, out)
	}
	f.srv.LogHeldBack()
	events := f.auditEvents(0)
	changed, keys, summary := 0, 0, 0
	for _, ev := range events {
		switch ev["event"] {
		case "audit.computer-name-changed":
			changed++
		case "audit.api-key-changed":
			keys++
		case "audit.changes-summary":
			summary++
			if ev["areas"] != "computer-name=80" || ev["count"].(float64) != 80 {
				t.Errorf("%v", ev)
			}
		}
	}
	if changed != changeAuditBurst || keys != 1 || summary != 1 {
		t.Errorf("%d lines of the limited route, %d key lines, %d summaries", changed, keys, summary)
	}
}

// An id that isn't the shape of one doesn't reach the log, even on a change that went through.
func TestAChangeWithAnOddTargetLogsNoTarget(t *testing.T) {
	f := newAuditFixture(t)
	const odd = "x\n{\"event\":\"audit.changes-summary\"}"
	h := f.srv.auditedRoute("voices", func(*http.Request, []byte, []byte) (auditedChange, bool) {
		return auditedChange{"rename", "voice", odd}, true
	}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	req := httptest.NewRequest("PATCH", "/_localbase/voices/x", strings.NewReader("{}"))
	h(httptest.NewRecorder(), req)
	events := f.auditEvents(0)
	if len(events) != 1 || events[0]["target_id"] != "" || events[0]["action"] != "rename" {
		t.Errorf("%v", events)
	}
}
