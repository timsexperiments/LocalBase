package contract

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// TestDifferentialUnlistedUser pins a deliberate difference (DESIGN §13.6 item 11):
// a browser session the policy grants nothing. Go refuses it everywhere. TS applies
// the permission only on routes that check again themselves, so models and inference
// answer it. Fake backends only: no real model, no network.
//
// Sessions are written to the database, because neither binary can be made to trust
// a fake HTTPS provider here. Neither implementation's sign-in consults the policy:
// that part is from reading the code and, for Go, TestOIDCUnauthorizedEmail.
func TestDifferentialUnlistedUser(t *testing.T) {
	if *implFlag != "both" {
		t.Skip("needs -impl=both")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("needs bun")
	}
	const (
		issuer = "https://accounts.google.com"
		origin = "https://lb.example.com"
		host   = "lb.example.com"
	)
	bins := filepath.Join(scratchDir(t), "bin")
	mustMkdirAll(bins, 0o700)
	fake := filepath.Join(bins, "llama-server")
	if out, err := exec.Command("go", "build", "-o", fake, "../internal/runtime/fakebackend").CombinedOutput(); err != nil {
		t.Fatalf("build fakebackend: %v %s", err, out)
	}
	raw, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := mustWriteFile(filepath.Join(bins, "sd-server"), raw, 0o700); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		status int
		body   string
	}
	run := func(impl string) map[string]answer {
		t.Helper()
		tmp := scratchDir(t)
		rootDir := filepath.Join(tmp, "root")
		if _, _, _, _, err := fixture(rootDir, freePort()); err != nil {
			t.Fatal(err)
		}
		// Small reserves, so the model fits a small CI runner and the chat reaches the fake: this
		// test is about permissions, and a 422 for memory would say nothing about them.
		small := filepath.Join(tmp, "small.toml")
		mustWriteFile(small, []byte("ctxSize = 2048\nctxMode = \"explicit\"\nparallel = 1\n\n[memory.systemReserve]\npercent = 1\nminimumGb = 0\n\n[memory.acceleratorReserve]\npercent = 1\nminimumGb = 0\n"), 0o600)
		if r := runCLI(t, impl, rootDir, "--non-interactive", "configure", "--config", small); r.code != 0 {
			t.Fatalf("%s configure: %s%s", impl, r.stdout, r.stderr)
		}
		policy := filepath.Join(tmp, "policy.json")
		b, _ := json.Marshal(auth.Policy{Roles: []auth.Role{
			{Name: "admin", Description: "Admin", Permissions: []string{"access:manage", "access:read", "models:read"}},
			{Name: "chatter", Description: "Chat", Permissions: []string{"inference:chat"}},
		}, Bindings: []auth.Binding{
			{Kind: "email", Role: "admin", Email: "owner@example.com"},
			{Kind: "email", Role: "chatter", Email: "chatter@example.com"},
		}})
		mustWriteFile(policy, b, 0o600)
		for _, args := range [][]string{
			{"--json", "access", "oidc", "add", "--id", "google", "--name", "Google", "--issuer", issuer, "--client-id", "cid", "--public-client", "--origin", origin},
			{"--json", "access", "policy", "apply", "--file", policy},
		} {
			if r := runCLI(t, impl, rootDir, args...); r.code != 0 {
				t.Fatalf("%s %v: %s%s", impl, args, r.stdout, r.stderr)
			}
		}
		st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
		if err != nil {
			t.Fatal(err)
		}
		session := func(subject, email string) string {
			t.Helper()
			token, err := auth.CreateSession(context.Background(), st, "owner-"+subject, auth.Identity{Issuer: issuer, Subject: subject, VerifiedEmail: email})
			if err != nil {
				t.Fatal(err)
			}
			return token
		}
		unlisted, listed, chatter := session("unlisted", "someone@example.com"), session("listed", "owner@example.com"), session("chatter", "chatter@example.com")
		st.Close()
		// The fake backends come first on PATH (TS finds its runtime there), and through
		// the override for Go.
		e := &e2e{t: t, root: rootDir, outDir: tmp, impl: impl,
			env: []string{"PATH=" + bins + string(os.PathListSeparator) + withoutRealBackends(os.Getenv("PATH")), "LOCALBASE_LLAMA_SERVER=" + fake}}
		e.serve()
		defer e.stop()
		send := func(method, path, cookie, body string) answer {
			t.Helper()
			req, _ := http.NewRequest(method, e.base+path, strings.NewReader(body))
			req.Host = host
			req.Header.Set("Cookie", auth.SessionCookie+"="+cookie)
			req.Header.Set("X-Localbase-Ui", "1")
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			return answer{resp.StatusCode, strings.TrimSpace(string(out))}
		}
		chat := `{"model":"` + chatModel + `","messages":[{"role":"user","content":"hi"}]}`
		return map[string]answer{
			"session":         send("GET", "/app/session", unlisted, ""),
			"access":          send("GET", "/app/api/_localbase/access-management", unlisted, ""),
			"keys":            send("GET", "/app/api/_localbase/api-keys", unlisted, ""),
			"management":      send("POST", "/app/api/_localbase/model-management", unlisted, `{"modelId":"x","action":"enable"}`),
			"models":          send("GET", "/app/api/_localbase/models", unlisted, ""),
			"image":           send("POST", "/app/api/v1/images/generations", unlisted, `{"prompt":"a"}`),
			"chat":            send("POST", "/app/api/v1/chat/completions", unlisted, chat),
			"role-can't-chat": send("POST", "/app/api/v1/chat/completions", listed, chat),
			"role-can-chat":   send("POST", "/app/api/v1/chat/completions", chatter, chat),
		}
	}
	ts, goImpl := run("ts"), run("go")
	// The same on both.
	for _, name := range []string{"session", "access", "keys", "management"} {
		if ts[name] != goImpl[name] {
			t.Errorf("%s:\n ts %d %.300s\n go %d %.300s", name, ts[name].status, ts[name].body, goImpl[name].status, goImpl[name].body)
		}
	}
	if s := goImpl["session"]; s.status != 200 || !strings.Contains(s.body, `"permissions":[]`) || !strings.Contains(s.body, `"authenticated":true`) {
		t.Errorf("session: %d %s", s.status, s.body)
	}
	for _, name := range []string{"access", "keys", "management"} {
		if goImpl[name].status != 403 {
			t.Errorf("%s: %d %s", name, goImpl[name].status, goImpl[name].body)
		}
	}
	// Go refuses; TS lets it past the permission check. Go must never loosen this.
	for _, name := range []string{"models", "image", "chat", "role-can't-chat"} {
		if goImpl[name].status != 403 {
			t.Errorf("go %s: %d %.300s", name, goImpl[name].status, goImpl[name].body)
		}
		if ts[name].status == 403 || ts[name].status == 401 {
			t.Logf("TS now refuses %s, so the recorded difference may be stale: %d", name, ts[name].status)
			t.Errorf("ts %s: %d %.300s", name, ts[name].status, ts[name].body)
		}
	}
	// The fake answered on both, proving no real model ran: its reply says so.
	for impl, answers := range map[string]map[string]answer{"ts": ts, "go": goImpl} {
		if a := answers["role-can-chat"]; a.status != 200 || !strings.Contains(a.body, "hello from") {
			t.Errorf("%s, a role that may chat: %d %.300s", impl, a.status, a.body)
		}
	}
	if a := ts["chat"]; a.status != 200 || !strings.Contains(a.body, "hello from") {
		t.Errorf("ts unlisted chat should reach the fake: %d %.300s", a.status, a.body)
	}
}
