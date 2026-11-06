package chaos

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// When the database cannot answer, someone holding something valid must not be told it is not:
// a client would drop a good key, and a person would be signed out. Each fault here is real, on the
// real file, from outside the gateway. The in-process sweep of every route is in
// internal/gateway/credential_lookup_test.go; this is what a client sees.

// databaseFaults are the ways of stopping the database answering. undo is nil where nothing can
// bring it back. In rollback-journal mode an exclusive lock makes a reader wait out the busy
// timeout and fail, so each answer under the lock takes about 5 s. WAL does not block readers
// that way, which is why every test first checks that the fault took (see faultTook).
var databaseFaults = []struct {
	name  string
	apply func(t *testing.T, ctx context.Context, g *lab.Gateway) (undo func())
}{
	{"locked", func(t *testing.T, ctx context.Context, g *lab.Gateway) func() {
		st, err := store.Open(dbPath(g))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		conn, err := st.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		held := true
		undo := func() {
			if held {
				held = false
				if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
					t.Errorf("releasing the lock: %v", err)
				}
			}
		}
		t.Cleanup(undo)
		return undo
	}},
	{"damaged", func(t *testing.T, _ context.Context, g *lab.Gateway) func() {
		f, err := os.OpenFile(dbPath(g), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt(bytes.Repeat([]byte{0xDB}, 64<<10), 0); err != nil {
			t.Fatal(err)
		}
		return nil
	}},
}

// faultTook stops a test whose fault did not stop the lookup: its answer says nothing about what
// happens when a lookup fails, and passing would be for the wrong reason.
func faultTook(t *testing.T, what string, status int) {
	t.Helper()
	if status >= 200 && status < 300 {
		t.Fatalf("%s: %d, so the database still answered and this test no longer tests a failed lookup. Under WAL an exclusive lock does not block readers: it needs a fault that does.", what, status)
	}
}

func TestAGoodKeyIsNeverToldItIsBad(t *testing.T) {
	knownBug(t, 609, "a key lookup that fails answers 401 invalid_api_key")
	for _, fault := range databaseFaults {
		t.Run(fault.name, func(t *testing.T) {
			ctx, g, c := start(t, lab.Options{})
			mustChat(t, ctx, c, g, lab.ChatSmall)
			undo := fault.apply(t, ctx, g)
			// The routes that dropped the lookup's error, and an inference route that never did.
			for _, p := range []struct{ method, path, body string }{
				{"GET", "/_localbase/api-keys", ""},
				{"POST", "/_localbase/api-keys", `{"name":"x"}`},
				{"GET", "/v1/no-such-route", ""},
				{"GET", "/v1/models", ""},
			} {
				what := p.method + " " + p.path
				ctype := ""
				if p.body != "" {
					ctype = "application/json"
				}
				r := c.Do(ctx, p.method, p.path, g.Keys.Admin, ctype, []byte(p.body))
				t.Logf("%s: %d %q Retry-After %q in %v", what, r.Status, r.Code, r.RetryAfter, r.Dur.Round(time.Millisecond))
				faultTook(t, what, r.Status)
				if r.Err != nil || r.Status != 503 || r.Code == "" || r.RetryAfter == "" {
					t.Errorf("a good key on %s, with the database %s: %d code %q Retry-After %q err %v, want 503 with both: %.300s", what, fault.name, r.Status, r.Code, r.RetryAfter, r.Err, r.Body)
				}
			}
			if undo == nil {
				return
			}
			undo()
			if r := c.Do(ctx, "GET", "/_localbase/api-keys", g.Keys.Admin, "", nil); r.Status != 200 {
				t.Errorf("the same key, once the database answers: %d %.300s", r.Status, r.Body)
			}
			// The check still refuses what is really bad.
			if r := c.Do(ctx, "GET", "/_localbase/api-keys", "lb_"+strings.Repeat("A", 32), "", nil); r.Status != 401 {
				t.Errorf("a key that does not exist, once the database answers: %d %.300s", r.Status, r.Body)
			}
		})
	}
}

// browser asks as a signed-in page does. It follows no redirect, since one to sign-in is the
// answer being looked for.
type browser struct {
	c      *http.Client
	base   string
	cookie *http.Cookie
}

type browserAnswer struct {
	status                         int
	location, retryAfter, hx, body string
	cleared                        bool
	took                           time.Duration
}

func (b browser) ask(t *testing.T, ctx context.Context, method, path string, headers map[string]string, body string) browserAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	began := time.Now()
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	a := browserAnswer{status: resp.StatusCode, location: resp.Header.Get("Location"), retryAfter: resp.Header.Get("Retry-After"),
		hx: resp.Header.Get("HX-Redirect"), body: string(text), took: time.Since(began)}
	for _, set := range resp.Cookies() {
		if set.Name == auth.LocalCookie && set.MaxAge < 0 {
			a.cleared = true
		}
	}
	return a
}

// signIn redeems a link as the sign-in page does, and returns the session's cookie.
func signIn(t *testing.T, ctx context.Context, g *lab.Gateway, b browser) *http.Cookie {
	t.Helper()
	token := localLink(t, ctx, g)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+auth.LocalOncePath, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.base)
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, set := range resp.Cookies() {
		if set.Name == auth.LocalCookie && set.Value != "" {
			return set
		}
	}
	t.Fatalf("no session from a good link: %d", resp.StatusCode)
	return nil
}

func localLink(t *testing.T, ctx context.Context, g *lab.Gateway) string {
	t.Helper()
	st, err := store.Open(dbPath(g))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token, err := auth.IssueLocalLink(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestASignedInBrowserIsNotSignedOutByTheDatabase(t *testing.T) {
	knownBug(t, 610, "a session lookup that fails signs the browser out")
	for _, fault := range databaseFaults {
		t.Run(fault.name, func(t *testing.T) {
			ctx, g, _ := start(t, lab.Options{})
			b := browser{c: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: g.Base}
			b.cookie = signIn(t, ctx, g, b)
			page := map[string]string{"Accept": "text/html"}
			if a := b.ask(t, ctx, http.MethodGet, "/app/chat", page, ""); a.status != 200 {
				t.Fatalf("a signed-in page before the fault: %d, to %q", a.status, a.location)
			}
			unused := localLink(t, ctx, g)
			undo := fault.apply(t, ctx, g)

			a := b.ask(t, ctx, http.MethodGet, "/app/chat", page, "")
			t.Logf("a page: %d to %q Retry-After %q cleared %v in %v", a.status, a.location, a.retryAfter, a.cleared, a.took.Round(time.Millisecond))
			faultTook(t, "a page", a.status)
			if a.status != 503 || a.retryAfter == "" || a.cleared || strings.Contains(a.location, "/app/login") {
				t.Errorf("a page, with the database %s: %d to %q, Retry-After %q, cookie cleared %v; want 503 with Retry-After and the cookie kept", fault.name, a.status, a.location, a.retryAfter, a.cleared)
			}

			a = b.ask(t, ctx, http.MethodGet, "/app/chat", map[string]string{"HX-Request": "true"}, "")
			t.Logf("a fragment: %d HX-Redirect %q Retry-After %q cleared %v", a.status, a.hx, a.retryAfter, a.cleared)
			if a.status != 503 || a.retryAfter == "" || a.hx != "" || a.cleared {
				t.Errorf("a fragment, with the database %s: %d, HX-Redirect %q, Retry-After %q, cookie cleared %v; want 503 with Retry-After and no redirect", fault.name, a.status, a.hx, a.retryAfter, a.cleared)
			}

			a = b.ask(t, ctx, http.MethodGet, "/app/session", map[string]string{"Origin": b.base}, "")
			t.Logf("/app/session: %d Retry-After %q cleared %v", a.status, a.retryAfter, a.cleared)
			if a.status == 401 || a.status == 403 || a.cleared {
				t.Errorf("/app/session, with the database %s: %d, cookie cleared %v", fault.name, a.status, a.cleared)
			}

			a = b.ask(t, ctx, http.MethodPost, "/app/logout", map[string]string{"X-Localbase-UI": "1", "Origin": b.base}, "")
			t.Logf("signing out: %d Retry-After %q cleared %v", a.status, a.retryAfter, a.cleared)
			if a.status != 503 || a.cleared {
				t.Errorf("signing out, with the database %s: %d, cookie cleared %v; want 503 with the cookie kept, since the session did not end", fault.name, a.status, a.cleared)
			}

			a = browser{c: b.c, base: b.base}.ask(t, ctx, http.MethodPost, auth.LocalOncePath,
				map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": b.base}, url.Values{"token": {unused}}.Encode())
			t.Logf("a good sign-in link: %d Retry-After %q", a.status, a.retryAfter)
			if a.status != 503 {
				t.Errorf("a good sign-in link, with the database %s: %d, want 503, not told it has expired", fault.name, a.status)
			}

			if undo == nil {
				return
			}
			undo()
			if a := b.ask(t, ctx, http.MethodGet, "/app/chat", page, ""); a.status != 200 {
				t.Errorf("the same browser, once the database answers: %d to %q", a.status, a.location)
			}
		})
	}
}
