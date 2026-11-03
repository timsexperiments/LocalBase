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
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

// usageOf reads the totals straight from the recorder, for one principal or all.
func usageOf(t *testing.T, s *Server, principal string) *usage.Report {
	t.Helper()
	now := time.Now()
	rep, err := s.Usage.Report(context.Background(), usage.Query{From: now.Add(-time.Hour), To: now.Add(2 * time.Hour), Principal: principal,
		Group: []string{"principal", "model", "route"}})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func oaKey(t *testing.T, s *Server, name string, scopes ...string) (id, secret string) {
	t.Helper()
	k, raw, err := s.Store.CreateAPIKey(t.Context(), name, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return k.ID, raw
}

// waitUsage waits for a request's count: it lands as the handler returns, and the client
// may see the end of the body a moment before that.
func waitUsage(t *testing.T, s *Server, principal string, want int64) *usage.Report {
	t.Helper()
	var rep *usage.Report
	if !waitFor(5*time.Second, func() bool { rep = usageOf(t, s, principal); return rep.Totals.Requests >= want }) {
		t.Fatalf("waiting for %d requests under %q: %+v", want, principal, rep.Totals)
	}
	return rep
}

// Planted values, where a client or a backend could put them. None may reach the table.
func TestUsageStoresNoContent(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key := oaKey(t, s, "keyname-is-a-label", auth.Permissions...)
	c := oaClient{t, ts.URL, key}
	const (
		prompt  = "PLANTEDPROMPT"
		reply   = "PLANTEDREPLY"
		user    = "PLANTEDUSER"
		unknown = "PLANTEDMODEL"
		file    = "PLANTEDFILENAME"
	)
	if status, _, b, _ := c.doBody(`{"model":"`+chatModel+`","user":"`+user+`","messages":[{"role":"user","content":"fake:reply `+reply+` `+prompt+`"}]}`, "POST", "/v1/chat/completions"); status != 200 || !strings.Contains(string(b), reply) {
		t.Fatalf("chat: %d %s", status, b)
	}
	if status, _, _, _ := c.doBody(`{"model":"`+unknown+`","messages":[{"role":"user","content":"hi"}]}`, "POST", "/v1/chat/completions"); status != 404 {
		t.Errorf("unknown model: %d", status)
	}
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	part, _ := w.CreateFormFile("file", file+".wav")
	part.Write(minimalWAV())
	w.WriteField("model", openAPISTTModel)
	w.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/audio/transcriptions", &form)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitUsage(t, s, "", 3)
	if err := s.Usage.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The unknown model counted, with no model: a string a client typed never lands here.
	var models []string
	rows, err := s.Store.DB.Query(`SELECT model_id FROM usage_hourly`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		rows.Scan(&m)
		models = append(models, m)
	}
	if !strings.Contains(strings.Join(models, ","), chatModel) {
		t.Errorf("models: %v", models)
	}
	empty := false
	for _, m := range models {
		empty = empty || m == ""
	}
	if !empty {
		t.Errorf("the unknown model's request should count with no model: %v", models)
	}

	// Every byte of the database, its WAL and shared memory.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(filepath.Join(s.Root.Dir, "local-base.db"+suffix))
		if err != nil {
			if suffix == "" {
				t.Fatal(err)
			}
			continue
		}
		for _, planted := range []string{prompt, reply, user, unknown, file, key} {
			if bytes.Contains(b, []byte(planted)) {
				t.Errorf("local-base.db%s holds %q", suffix, planted)
			}
		}
	}
}

// A browser identity is its owner hash, and nothing else, anywhere (DESIGN §32.1).
func TestUsageNoIdentity(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	const email, name = "planted.person@example.test", "Planted Person"
	owner := "browser:0123456789abcdef"
	person := &auth.Principal{Kind: "browser-session", ID: owner, Name: email, Permissions: auth.Permissions,
		Identity: &auth.Identity{Issuer: "https://issuer.example", Subject: name, VerifiedEmail: email}}
	res := s.Call(t.Context(), person, "POST", "/v1/chat/completions", "application/json", []byte(chatBody("hi")))
	if res.Status != 200 {
		t.Fatalf("chat: %d %s", res.Status, res.Body)
	}
	waitUsage(t, s, owner, 1)
	if err := s.Usage.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var kind, id string
	if err := s.Store.DB.QueryRow(`SELECT principal_kind, principal_id FROM usage_hourly`).Scan(&kind, &id); err != nil {
		t.Fatal(err)
	}
	if kind != "browser" || id != owner {
		t.Errorf("stored %q %q", kind, id)
	}
	for _, suffix := range []string{"", "-wal"} {
		b, _ := os.ReadFile(filepath.Join(s.Root.Dir, "local-base.db"+suffix))
		for _, planted := range []string{email, name} {
			if bytes.Contains(b, []byte(planted)) {
				t.Errorf("local-base.db%s holds %q", suffix, planted)
			}
		}
	}
	for _, q := range []string{"", "?scope=all", "?scope=all&group=principal", "?scope=all&group=principal&group=model&group=route", "?group=principal"} {
		res := s.Call(t.Context(), person, "GET", "/_localbase/usage"+q, "", nil)
		if res.Status != 200 {
			t.Fatalf("%s: %d %s", q, res.Status, res.Body)
		}
		if bytes.Contains(res.Body, []byte(email)) || bytes.Contains(res.Body, []byte(name)) {
			t.Errorf("%s: the answer names the person: %s", q, res.Body)
		}
	}
	// Even a browser id that is not already a hash is stored as one.
	raw := &auth.Principal{Kind: "browser-session", ID: "not-a-hash@example.test", Permissions: auth.Permissions}
	if k, got, ok := s.usagePrincipal(raw); !ok || k != "browser" || strings.Contains(got, "@") || !strings.HasPrefix(got, "browser:") {
		t.Errorf("%q %q %v", k, got, ok)
	}
}

// DESIGN §32.2, row by row, through the real routes.
func TestUsageOutcomes(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	id, key := oaKey(t, s, "k", auth.DefaultKeyScopes...)
	c := oaClient{t, ts.URL, key}
	check := func(what string, send func(), requests, refused, failed, cancelled int64) {
		t.Helper()
		before := usageOf(t, s, id).Totals
		send()
		if !waitFor(5*time.Second, func() bool {
			now := usageOf(t, s, id).Totals
			return now.Requests-before.Requests >= requests
		}) {
			t.Errorf("%s: never counted: %+v", what, usageOf(t, s, id).Totals)
			return
		}
		time.Sleep(50 * time.Millisecond) // anything counted twice would land by now
		now := usageOf(t, s, id).Totals
		got := [4]int64{now.Requests - before.Requests, now.Refused - before.Refused, now.Failed - before.Failed, now.Cancelled - before.Cancelled}
		if want := [4]int64{requests, refused, failed, cancelled}; got != want {
			t.Errorf("%s: requests, refused, failed, cancelled = %v, want %v", what, got, want)
		}
	}
	check("a 200", func() { c.doBody(chatBody("hi"), "POST", "/v1/chat/completions") }, 1, 0, 0, 0)
	check("a bad body", func() { c.doBody(`{"model":""}`, "POST", "/v1/chat/completions") }, 1, 1, 0, 0)
	check("an unknown model", func() {
		c.doBody(`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, "POST", "/v1/chat/completions")
	}, 1, 1, 0, 0)
	check("a model that cannot be chatted with", func() {
		c.doBody(`{"model":"`+embeddingModel+`","messages":[{"role":"user","content":"hi"}]}`, "POST", "/v1/chat/completions")
	}, 1, 1, 0, 0)
	check("a stream that fails after it started", func() {
		c.doBody(`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:truncated"}]}`, "POST", "/v1/chat/completions")
	}, 1, 0, 1, 0)
	check("a client that goes away", func() {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 200"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.ReadFull(resp.Body, make([]byte, 16))
		cancel()
	}, 1, 0, 0, 1)
	// A detached run counts once, as its inner request. The outer POST is not a /v1 route.
	check("a detached run", func() {
		status, _, body, _ := c.doBody(chatBody("hi"), "POST", "/_localbase/runs/v1/chat/completions")
		if status != 202 {
			t.Fatalf("run: %d %s", status, body)
		}
	}, 1, 0, 0, 0)
	check("a detached run refused before the queue", func() { c.doBody(`{"model":""}`, "POST", "/_localbase/runs/v1/chat/completions") }, 1, 1, 0, 0)
	// A UI call counts as the person, once.
	person := &auth.Principal{Kind: "browser-session", ID: "browser:abc", Permissions: auth.Permissions}
	if res := s.Call(t.Context(), person, "POST", "/v1/chat/completions", "application/json", []byte(chatBody("hi"))); res.Status != 200 {
		t.Errorf("UI call: %d", res.Status)
	}
	if got := waitUsage(t, s, "browser:abc", 1).Totals.Requests; got != 1 {
		t.Errorf("UI call counted %d times", got)
	}

	// Not counted: no valid key, and what is not an inference route.
	before := usageOf(t, s, "").Totals.Requests
	(oaClient{t, ts.URL, ""}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	(oaClient{t, ts.URL, "lb_wrong"}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	c.do("GET", "/v1/models")
	c.do("GET", "/_localbase/usage")
	(oaClient{t, ts.URL, ""}).do("GET", "/health")
	time.Sleep(50 * time.Millisecond)
	if got := usageOf(t, s, "").Totals.Requests; got != before {
		t.Errorf("%d not-counted requests were counted", got-before)
	}
	// A key without the scope is identified, and refused: that counts, under it.
	_, reader := oaKey(t, s, "reader", "models:read")
	(oaClient{t, ts.URL, reader}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	if !waitFor(5*time.Second, func() bool { return usageOf(t, s, "").Totals.Requests == before+1 }) {
		t.Errorf("a key refused for its scope should count: %d", usageOf(t, s, "").Totals.Requests-before)
	}
}

// A job counts once, when it ends, however long it runs and whoever polls it.
func TestUsageVideoJobCountsOnce(t *testing.T) {
	g := newVideoGateway(t)
	s := runServer(g.ts)
	if got := post(t, g.ts, "/v1/videos", `{"model":""}`); got.status != 400 {
		t.Fatalf("a bad create: %d", got.status)
	}
	if got := usageOf(t, s, "no-auth").Totals; got.Requests != 1 || got.Refused != 1 {
		t.Errorf("a refused create counts as refused: %+v", got)
	}
	job := g.create(t, "a red fox in the snow")
	id := job["id"].(string)
	// Accepted but not finished: it has not counted yet.
	if done := g.waitStatus(t, id); done["status"] != "completed" {
		t.Fatalf("job: %v", done)
	}
	g.get(t, "/v1/videos/"+id)
	g.get(t, "/v1/videos/"+id+"/content")
	rep := waitUsage(t, s, "no-auth", 2)
	if rep.Totals.Requests != 2 || rep.Totals.Failed != 0 || rep.Totals.Cancelled != 0 {
		t.Errorf("after one completed job: %+v", rep.Totals)
	}
	if rep.Totals.Latency.P50 == nil {
		t.Errorf("latency: %+v", rep.Totals.Latency)
	}
	var modelled bool
	for _, group := range rep.Groups {
		modelled = modelled || (group.Model != nil && *group.Model == videoModel && *group.Route == "video")
	}
	if !modelled {
		t.Errorf("groups: %+v", rep.Groups)
	}
	for state, want := range map[video.State]int{video.Completed: 200, video.Failed: 500, video.Cancelled: 499} {
		if got := videoJobStatus(state); got != want {
			t.Errorf("%s: %d, want %d", state, got, want)
		}
	}
}

func TestUsageScopes(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	aID, a := oaKey(t, s, "a", auth.DefaultKeyScopes...)
	bID, b := oaKey(t, s, "b", auth.DefaultKeyScopes...)
	_, manager := oaKey(t, s, "manager", "models:manage")
	_, sysread := oaKey(t, s, "sysread", "system:read")
	_, accessOnly := oaKey(t, s, "access", "access:manage")
	(oaClient{t, ts.URL, a}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	(oaClient{t, ts.URL, b}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	(oaClient{t, ts.URL, b}).doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	waitUsage(t, s, "", 3)

	read := func(key, query string) (int, usage.Report) {
		t.Helper()
		status, _, body, _ := (oaClient{t, ts.URL, key}).do("GET", "/_localbase/usage"+query)
		var rep usage.Report
		json.Unmarshal(body, &rep)
		return status, rep
	}
	// A key sees only its own, however it asks.
	if status, rep := read(a, ""); status != 200 || rep.Totals.Requests != 1 || rep.Scope != "self" {
		t.Errorf("own: %d %+v", status, rep.Totals)
	}
	if status, rep := read(a, "?group=principal"); status != 200 || len(rep.Groups) != 1 || rep.Groups[0].Principal.ID != aID {
		t.Errorf("own, grouped: %d %+v", status, rep.Groups)
	}
	if status, _ := read(a, "?principal="+aID); status != 200 {
		t.Errorf("naming itself: %d", status)
	}
	for _, q := range []string{"?scope=all", "?principal=" + bID, "?scope=all&principal=" + bID, "?scope=all&group=principal"} {
		if status, _ := read(a, q); status != 403 {
			t.Errorf("a key asking %s: %d, want 403, not an empty answer", q, status)
		}
	}
	// models:manage and system:read see everything. access:manage alone sees its own.
	for _, key := range []string{manager, sysread} {
		if status, rep := read(key, "?scope=all&group=principal"); status != 200 || rep.Totals.Requests != 3 || len(rep.Groups) != 2 || rep.Scope != "all" {
			t.Errorf("all: %d %+v", status, rep)
		}
		if status, rep := read(key, "?scope=all&principal="+bID); status != 200 || rep.Totals.Requests != 2 {
			t.Errorf("all, one: %d %+v", status, rep.Totals)
		}
		// Its own view is still its own.
		if status, rep := read(key, ""); status != 200 || rep.Totals.Requests != 0 {
			t.Errorf("self: %d %+v", status, rep.Totals)
		}
	}
	if status, _ := read(accessOnly, "?scope=all"); status != 403 {
		t.Errorf("access:manage alone reading all: %d", status)
	}
	if status, _, _, _ := (oaClient{t, ts.URL, ""}).do("GET", "/_localbase/usage"); status != 401 {
		t.Errorf("no key: %d", status)
	}
	// A key's name is read when usage is read, for those allowed to see it.
	if _, rep := read(manager, "?scope=all&group=principal"); rep.Groups[0].Principal.Name != "b" {
		t.Errorf("name: %+v", rep.Groups[0].Principal)
	}
}

// DESIGN §32.5, row by row.
func TestUsageOwnerCanClear(t *testing.T) {
	clear := []byte(`{"action":"clear"}`)
	ts := openAPIGateway(t)
	s := runServer(ts)
	post := func(key string) int {
		t.Helper()
		status, _, _, _ := (oaClient{t, ts.URL, key}).doBody(string(clear), "POST", "/_localbase/usage")
		return status
	}
	call := func(p *auth.Principal) int {
		t.Helper()
		return s.Call(t.Context(), p, "POST", "/_localbase/usage", "application/json", clear).Status
	}
	for name, c := range map[string]struct {
		scopes []string
		want   int
	}{
		"the first-run key":         {auth.DefaultKeyScopes, 403},
		"inference and models:read": {[]string{"inference:chat", "inference:embeddings", "models:read"}, 403},
		"a Connect key":             {[]string{"inference:chat", "models:read"}, 403},
		"models:manage alone":       {[]string{"models:manage"}, 403},
		"system:read alone":         {[]string{"system:read"}, 403},
		"keys:manage alone":         {[]string{"keys:manage"}, 403},
		"a key given system:manage": {[]string{"system:manage"}, 200},
		"a key given access:manage": {[]string{"access:manage"}, 200},
	} {
		_, key := oaKey(t, s, strings.ReplaceAll(name, " ", "-"), c.scopes...)
		if got := post(key); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if got := post(""); got != 401 {
		t.Errorf("no key: %d", got)
	}
	browser := func(perms ...string) *auth.Principal {
		return &auth.Principal{Kind: "browser-session", ID: "browser:owner", Name: "owner@example.test", Permissions: perms,
			Identity: &auth.Identity{Issuer: "i", Subject: "s", VerifiedEmail: "owner@example.test"}}
	}
	for name, c := range map[string]struct {
		p    *auth.Principal
		want int
	}{
		"a provider session with access:manage only": {browser("access:manage"), 200},
		"a provider session with system:manage":      {browser("system:manage"), 200},
		// As the owner's role is on the live gateway: no system:* at all.
		"the owner's role":                         {browser("access:manage", "access:read", "keys:manage", "keys:read", "models:manage", "models:read", "inference:chat"), 200},
		"a default provider session":               {browser(auth.DefaultBrowserPermissions...), 403},
		"a local session, from the first-run link": {&auth.Principal{Kind: "browser-session", ID: auth.LocalOwnerID, Permissions: auth.Permissions}, 200},
	} {
		if got := call(c.p); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if !UsageCanChange(true, auth.Anonymous) {
		t.Error("--no-auth")
	}
}

// The instance token is this machine's CLI: writes only, loopback only (DESIGN §32.4).
func TestUsageInstanceToken(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	release, err := s.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	token := s.Root.Instance().InstanceToken
	host := ts.Listener.Addr().String()
	send := func(method, body, requestHost string, headers ...string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+"/_localbase/usage", strings.NewReader(body))
		req.Host = requestHost
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	const hdr, clear = "X-Localbase-Instance-Token", `{"action":"clear"}`
	if got := send("POST", clear, host, hdr, token); got != 200 {
		t.Errorf("the token: %d", got)
	}
	if got := send("POST", clear, host, hdr, "wrong"); got != 401 {
		t.Errorf("a wrong token: %d", got)
	}
	if got := send("GET", "", host, hdr, token); got != 401 {
		t.Errorf("reading with the token: %d; it is good for changes only", got)
	}
	for _, c := range [][]string{{"X-Forwarded-For", "1.2.3.4"}, {"Cf-Ray", "x"}, {"Forwarded", "for=1.2.3.4"}, {"Origin", "https://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		if got := send("POST", clear, host, hdr, token, c[0], c[1]); got != 401 {
			t.Errorf("with %s: %d", c[0], got)
		}
	}
	if got := send("POST", clear, "gateway.example", hdr, token); got != 401 {
		t.Errorf("a public Host: %d", got)
	}
	// From another computer: the socket says so, whatever the Host claims.
	req := httptest.NewRequest("POST", "/_localbase/usage", strings.NewReader(clear))
	req.Host, req.RemoteAddr = host, "192.168.1.9:5000"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, ts.Listener.Addr()))
	req.Header.Set(hdr, token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("from another computer: %d", w.Code)
	}
}

func TestUsageChanges(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key := oaKey(t, s, "admin", "system:manage", "system:read")
	c := oaClient{t, ts.URL, key}
	c.doBody(chatBody("hi"), "POST", "/v1/chat/completions") // refused for its scope, and counted
	waitUsage(t, s, "", 1)
	for _, bad := range []string{`{}`, `{"action":"nope"}`, `{"action":"clear","days":3}`, `{"action":"clear","before":"yesterday"}`, `{"action":"set-retention"}`,
		`{"action":"set-retention","days":0}`, `{"action":"set-retention","days":366}`, `{"action":"set-retention","days":1.5}`, `{"action":"set-recording"}`,
		`{"action":"set-recording","enabled":"yes"}`, `{"action":"clear","extra":1}`, `not json`} {
		if status, _, _, _ := c.doBody(bad, "POST", "/_localbase/usage"); status != 400 {
			t.Errorf("%s: %d", bad, status)
		}
	}
	if status, _, _, _ := c.doBody(`{"action":"clear","before":"`+strings.Repeat("x", 5000)+`"}`, "POST", "/_localbase/usage"); status != 413 {
		t.Errorf("too large: %d", status)
	}
	status, _, body, _ := c.doBody(`{"action":"set-retention","days":7}`, "POST", "/_localbase/usage")
	if status != 200 || !bytes.Contains(body, []byte(`"retentionDays":7`)) {
		t.Errorf("retention: %d %s", status, body)
	}
	status, _, body, _ = c.doBody(`{"action":"set-recording","enabled":false}`, "POST", "/_localbase/usage")
	if status != 200 || !bytes.Contains(body, []byte(`"recording":false`)) {
		t.Errorf("recording: %d %s", status, body)
	}
	before := usageOf(t, s, "").Totals.Requests
	c.doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	time.Sleep(50 * time.Millisecond)
	if got := usageOf(t, s, "").Totals.Requests; got != before {
		t.Errorf("counted with recording off: %d", got-before)
	}
	if status, _, _, _ := c.doBody(`{"action":"set-recording","enabled":true}`, "POST", "/_localbase/usage"); status != 200 {
		t.Errorf("back on: %d", status)
	}
	// Clearing takes what is in memory too.
	c.doBody(chatBody("hi"), "POST", "/v1/chat/completions")
	waitFor(5*time.Second, func() bool { return usageOf(t, s, "").Totals.Requests > before })
	status, _, body, _ = c.doBody(`{"action":"clear"}`, "POST", "/_localbase/usage")
	if status != 200 || !bytes.Contains(body, []byte(`"cleared":true`)) {
		t.Errorf("clear: %d %s", status, body)
	}
	s.Usage.Flush(t.Context())
	if got := usageOf(t, s, "").Totals.Requests; got != 0 {
		t.Errorf("after clearing: %d", got)
	}
}

// Audit events carry ids only.
func TestUsageAudit(t *testing.T) {
	logs := &syncBuffer{}
	testLog = logs
	defer func() { testLog = nil }()
	ts := openAPIGateway(t)
	testLog = nil
	s := runServer(ts)
	id, key := oaKey(t, s, "secretlabel", "system:manage")
	if status, _, body, _ := (oaClient{t, ts.URL, key}).doBody(`{"action":"clear"}`, "POST", "/_localbase/usage"); status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `"audit.usage-changed"`) {
			line = l
		}
	}
	if !strings.Contains(line, `"action":"clear"`) || !strings.Contains(line, id) {
		t.Errorf("no audit event: %s", logs.String())
	}
	if strings.Contains(line, key) || strings.Contains(line, "secretlabel") {
		t.Errorf("the audit event holds a key or its label: %s", line)
	}
	// The whole log never holds the key itself, as for every other route.
	if strings.Contains(logs.String(), key) {
		t.Errorf("the log holds the key")
	}
}

func TestUsageBrowserRoute(t *testing.T) {
	for _, c := range []struct {
		method string
		ok     bool
	}{{"GET", true}, {"POST", true}, {"DELETE", false}, {"PUT", false}} {
		if got := allowedUIRoute(c.method, "/_localbase/usage"); got != c.ok {
			t.Errorf("%s: %v", c.method, got)
		}
	}
	r := httptest.NewRequest("GET", "/app/api/_localbase/usage?range=7d&scope=all&group=model&group=route&secret=x", nil)
	got := forwardedQuery("/_localbase/usage", r)
	for _, want := range []string{"range=7d", "scope=all", "group=model", "group=route"} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded %q: missing %s", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("forwarded %q", got)
	}
}
