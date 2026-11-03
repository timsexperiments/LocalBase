package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Characterization, inventory item 3: the browser edges nothing exercised.

func TestSameOriginStream(t *testing.T) {
	b := browserFor(t, testStore(t), Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	mk := func(method, host string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(method, "/app/api/_localbase/runs/r/events", nil)
		r.Host = host
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	for name, c := range map[string]struct {
		r    *http.Request
		want bool
	}{
		// An EventSource cannot add the UI marker, so none is needed here.
		"no markers at all":   {mk("GET", "lb.example.com", nil), true},
		"same origin":         {mk("GET", "lb.example.com", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": testOrigin}), true},
		"another host":        {mk("GET", "other.example.com", nil), false},
		"not a GET":           {mk("POST", "lb.example.com", map[string]string{"Origin": testOrigin}), false},
		"cross site":          {mk("GET", "lb.example.com", map[string]string{"Sec-Fetch-Site": "cross-site"}), false},
		"same site is not it": {mk("GET", "lb.example.com", map[string]string{"Sec-Fetch-Site": "same-site"}), false},
		"another origin":      {mk("GET", "lb.example.com", map[string]string{"Origin": "https://evil.example.com"}), false},
		"http origin":         {mk("GET", "lb.example.com", map[string]string{"Origin": "http://lb.example.com"}), false},
	} {
		if got := b.SameOriginStream(c.r); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	off := &Browser{}
	if off.SameOriginStream(mk("GET", "lb.example.com", nil)) {
		t.Error("with browser access off")
	}
}

// The page never holds the token: it stays in the fragment, and the script that
// posts it is pinned by hash. Showing the page spends nothing.
func TestMagicLinkPage(t *testing.T) {
	b := browserFor(t, testStore(t), Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	w := httptest.NewRecorder()
	b.MagicLinkPage(w)
	h := w.Result().Header
	body := w.Body.String()
	for name, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "same-origin", "X-Content-Type-Options": "nosniff"} {
		if h.Get(name) != want {
			t.Errorf("%s: %q", name, h.Get(name))
		}
	}
	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no script: %s", body)
	}
	sum := sha256.Sum256([]byte(m[1]))
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("the CSP does not pin the script: %s", csp)
	}
	if !strings.Contains(body, `action="`+MagicCallbackPath+`"`) || !strings.Contains(m[1], "location.hash") || !strings.Contains(m[1], "history.replaceState") {
		t.Errorf("the page: %s", body)
	}
}

func magicFixture(t *testing.T) (*Browser, context.Context) {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	b := browserFor(t, st, Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	if err := ApplyPolicy(ctx, st, &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}}},
		Bindings: []Binding{{Kind: "subject", Role: "chat", Issuer: "https://id.example.com", Subject: "owner"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "friend@example.com", []string{"chat"}, MagicIssuer(testOrigin)); err != nil {
		t.Fatal(err)
	}
	return b, ctx
}

func TestRevokeMagicLink(t *testing.T) {
	b, ctx := magicFixture(t)
	issuer := MagicIssuer(testOrigin)
	token, _, err := IssueMagicLink(ctx, b.Store, "friend@example.com", MagicIssuer(testOrigin))
	if err != nil || token == "" {
		t.Fatalf("issue: %q %v", token, err)
	}
	// A malformed or unknown token changes nothing.
	RevokeMagicLink(ctx, b.Store, "not-a-token")
	RevokeMagicLink(ctx, b.Store, RandomToken(32))
	var n int
	b.Store.DB.QueryRow(`SELECT count(*) FROM auth_magic_link_tokens`).Scan(&n)
	if n != 1 {
		t.Fatalf("stored links: %d", n)
	}
	RevokeMagicLink(ctx, b.Store, token)
	if id, err := ConsumeMagicLink(ctx, b.Store, token, issuer); err != nil || id != nil {
		t.Errorf("a revoked link: %+v %v", id, err)
	}
	// A revoked link does not hold the cooldown: the person can ask again at once.
	again, _, err := IssueMagicLink(ctx, b.Store, "friend@example.com", MagicIssuer(testOrigin))
	if err != nil || again == "" {
		t.Errorf("after a revoke: %q %v", again, err)
	}
}

func TestMagicLinkExpired(t *testing.T) {
	b, ctx := magicFixture(t)
	token, _, _ := IssueMagicLink(ctx, b.Store, "friend@example.com", MagicIssuer(testOrigin))
	b.Store.DB.Exec(`UPDATE auth_magic_link_tokens SET expires_at = ?`, time.Now().UTC().Add(-time.Second).Format("2006-01-02T15:04:05.000Z"))
	if id, err := ConsumeMagicLink(ctx, b.Store, token, MagicIssuer(testOrigin)); err != nil || id != nil {
		t.Errorf("an expired link: %+v %v", id, err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("!", 43)} {
		if id, _ := ConsumeMagicLink(ctx, b.Store, bad, MagicIssuer(testOrigin)); id != nil {
			t.Errorf("%q redeemed", bad)
		}
	}
}

// A failed delivery revokes the link and logs without the token or address.
func TestMagicLinkDeliveryFailure(t *testing.T) {
	b, _ := magicFixture(t)
	var logs safeBuffer
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan string, 1)
	r := httptest.NewRequest("POST", MagicRequestPath+"?provider=email", strings.NewReader(url.Values{"email": {"friend@example.com"}}.Encode()))
	r.Host = "lb.example.com"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.RequestMagicLink(w, r, func(to, link string) error {
		done <- link
		return errors.New("the mail server said no")
	})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Check your email") {
		t.Errorf("the answer must not reveal a failure: %d %s", w.Code, w.Body)
	}
	var link string
	select {
	case link = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("never delivered")
	}
	token := link[strings.Index(link, "#")+1:]
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		b.Store.DB.QueryRow(`SELECT count(*) FROM auth_magic_link_tokens`).Scan(&n)
		if n == 0 && strings.Contains(logs.String(), "auth.magic-link-delivery-failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not revoked, or not logged: %d rows, %s", n, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := logs.String(); strings.Contains(got, token) || strings.Contains(got, "friend@example.com") {
		t.Errorf("the log holds a credential or address: %s", got)
	}
}

// A request naming another provider, or none, sends nothing and looks the same.
func TestMagicLinkWrongProvider(t *testing.T) {
	b, _ := magicFixture(t)
	for _, target := range []string{MagicRequestPath, MagicRequestPath + "?provider=other"} {
		sent := make(chan struct{}, 1)
		r := httptest.NewRequest("POST", target, strings.NewReader(url.Values{"email": {"friend@example.com"}}.Encode()))
		r.Host = "lb.example.com"
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		b.RequestMagicLink(w, r, func(string, string) error { sent <- struct{}{}; return nil })
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Check your email") {
			t.Errorf("%s: %d", target, w.Code)
		}
		select {
		case <-sent:
			t.Errorf("%s sent a link", target)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// safeBuffer is a log the delivery goroutine and the test share.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
