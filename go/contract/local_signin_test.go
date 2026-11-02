package contract

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var localLinkRe = regexp.MustCompile(`http://127\.0\.0\.1:\d+/app/login/once#[A-Za-z0-9_-]{43}`)

// The whole path on the real binary with nothing configured: first-run link once,
// the page, the session, the app, and the refusal once a provider exists. Go only:
// TS has no local sign-in (DESIGN §25).
func TestFirstRunLocalSignIn(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	serveOutput := func() string {
		t.Helper()
		logs, _ := filepath.Glob(filepath.Join(tmp, "serve-*.log"))
		var all strings.Builder
		for _, f := range logs {
			b, _ := os.ReadFile(f)
			all.Write(b)
		}
		return all.String()
	}
	// The API on a signed-out gateway keeps its JSON; a navigation gets the page.
	e.serve("--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
	defer func() { e.stop() }()
	deadline := time.Now().Add(5 * time.Second)
	for len(localLinkRe.FindAllString(serveOutput(), -1)) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	links := localLinkRe.FindAllString(serveOutput(), -1)
	if len(links) != 1 || !strings.HasPrefix(links[0], e.base) {
		t.Fatalf("first-run links %v, gateway %s:\n%s", links, e.base, serveOutput())
	}
	if !strings.Contains(serveOutput(), "does not work from a phone") {
		t.Error("the first-run message should say a phone needs an identity provider")
	}
	link := links[0]
	page, token, _ := strings.Cut(link, "#")

	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, h map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 64<<10)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp, b.String()
	}
	html := map[string]string{"Accept": "text/html"}
	if resp, body := get(e.base+"/app/chat", html); resp.StatusCode != 401 || !strings.Contains(body, "local-base access login-link") {
		t.Errorf("/app/chat signed out: %d %.200s", resp.StatusCode, body)
	}
	if resp, body := get(e.base+"/app/login", html); resp.StatusCode != 403 || !strings.Contains(body, "local-base access login-link") {
		t.Errorf("/app/login: %d %.200s", resp.StatusCode, body)
	}
	if resp, body := get(e.base+"/app/login", html); !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || strings.Contains(body, "ui_access_denied") {
		t.Errorf("/app/login is not a page: %.200s", body)
	}
	if resp, body := get(e.base+"/app/login", nil); resp.StatusCode != 401 || !strings.Contains(body, "ui_access_denied") {
		t.Errorf("/app/login as an API client: %d %.200s", resp.StatusCode, body)
	}
	// The redeem page's script matches the hash its CSP pins, and it holds no token.
	resp, body := get(page, html)
	if resp.StatusCode != 200 || !strings.Contains(body, "Signing you in") || strings.Contains(body, token) {
		t.Fatalf("redeem page: %d %.300s", resp.StatusCode, body)
	}
	script := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(body)
	if script == nil {
		t.Fatalf("no script: %.400s", body)
	}
	sum := sha256.Sum256([]byte(script[1]))
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Errorf("the CSP does not pin the page's script: %s", csp)
	}
	// Redeem, then the app and its API as a browser.
	form := url.Values{"token": {token}}.Encode()
	req, _ := http.NewRequest("POST", page, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	post, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != 303 || post.Header.Get("Location") != "/app" || len(post.Cookies()) != 1 {
		t.Fatalf("redeem: %d %v", post.StatusCode, post.Cookies())
	}
	ck := post.Cookies()[0]
	session := map[string]string{"Cookie": ck.Name + "=" + ck.Value, "Accept": "text/html"}
	if resp, body := get(e.base+"/app/chat", session); resp.StatusCode != 200 {
		t.Errorf("/app/chat signed in: %d %.200s", resp.StatusCode, body)
	}
	api := map[string]string{"Cookie": ck.Name + "=" + ck.Value, "X-Localbase-Ui": "1"}
	if resp, body := get(e.base+"/app/session", api); resp.StatusCode != 200 || !strings.Contains(body, `"localSession":true`) {
		t.Errorf("/app/session: %d %s", resp.StatusCode, body)
	}
	proxied := map[string]string{"Cookie": ck.Name + "=" + ck.Value, "X-Localbase-Ui": "1", "Cf-Connecting-Ip": "203.0.113.9"}
	if resp, _ := get(e.base+"/app/session", proxied); resp.StatusCode == 200 {
		t.Error("a tunnel-shaped request used the session")
	}
	// Listing and ending sessions from the CLI, never showing a token.
	listed := e.json("access", "sessions", "list")["sessions"].([]any)
	if len(listed) != 1 {
		t.Fatalf("sessions: %v", listed)
	}
	id := listed[0].(map[string]any)["id"].(string)
	if r := e.cli(nil, "--json", "access", "login-link"); r.code != 0 || !localLinkRe.MatchString(r.stdout) {
		t.Errorf("login-link: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := e.cli(nil, "--non-interactive", "access", "sessions", "revoke", id); r.code != 0 {
		t.Errorf("revoke: %s", r.stderr)
	}
	if resp, _ := get(e.base+"/app/session", api); resp.StatusCode == 200 {
		t.Error("a revoked session worked")
	}
	if strings.Contains(serveOutput(), ck.Value) {
		t.Error("the gateway's output holds the session token")
	}

	// Restarting does not show a first-run link again.
	e.stop()
	before := len(localLinkRe.FindAllString(serveOutput(), -1))
	e.serve("--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
	time.Sleep(300 * time.Millisecond)
	if after := len(localLinkRe.FindAllString(serveOutput(), -1)); after != before {
		t.Errorf("the first-run link came again: %d then %d", before, after)
	}
	events, _ := os.ReadFile(filepath.Join(e.root, "logs", "events.jsonl"))
	if !strings.Contains(string(events), "audit.local-login-issued") || strings.Contains(string(events), token) {
		t.Errorf("audit log: issued event missing, or it holds the token")
	}
	e.stop()

	// With a provider configured, a link needs --force, and none shows at first run.
	if r := e.cli(nil, "--non-interactive", "access", "cloudflare", "--team-domain", "team.cloudflareaccess.com", "--audience", strings.Repeat("a", 32), "--origin", "https://localbase.example.com"); r.code != 0 {
		t.Fatalf("configure a provider: %s%s", r.stdout, r.stderr)
	}
	r := e.cli(nil, "--non-interactive", "access", "login-link")
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "--force") || localLinkRe.MatchString(r.stdout+r.stderr) {
		t.Errorf("no --force: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := e.cli(nil, "--non-interactive", "access", "login-link", "--force"); r.code != 0 || !localLinkRe.MatchString(r.stdout+r.stderr) {
		t.Errorf("--force: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := e.cli(nil, "--json", "access", "login-link", "--revoke"); r.code != 0 {
		t.Errorf("--revoke: %s", r.stderr)
	}
}
