package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLinkSingleUse(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	token, err := IssueLocalLink(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ConsumeLocalLink(ctx, st, RandomToken(32)); ok || err != nil {
		t.Errorf("a wrong token: %v %v", ok, err)
	}
	if ok, err := ConsumeLocalLink(ctx, st, token); !ok || err != nil {
		t.Errorf("first use: %v %v", ok, err)
	}
	if ok, _ := ConsumeLocalLink(ctx, st, token); ok {
		t.Error("second use succeeded")
	}
}

func TestLoginLinkExpires(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	token, _ := IssueLocalLink(ctx, st)
	var exp, created int64
	st.DB.QueryRow(`SELECT expires_at, created_at FROM local_login_links`).Scan(&exp, &created)
	if time.Duration(exp-created)*time.Millisecond != LocalLinkTTL {
		t.Errorf("lifetime: %d ms", exp-created)
	}
	st.DB.Exec(`UPDATE local_login_links SET expires_at = ?`, ms(time.Now().Add(-time.Second)))
	if ok, _ := ConsumeLocalLink(ctx, st, token); ok {
		t.Error("an expired link worked")
	}
}

func TestLoginLinkHashedAtRest(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	link, _ := IssueLocalLink(ctx, st)
	cookie, _, err := CreateLocalSession(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	var a, b string
	st.DB.QueryRow(`SELECT token_hash FROM local_login_links`).Scan(&a)
	st.DB.QueryRow(`SELECT token_hash FROM local_sessions`).Scan(&b)
	if a != hashToken(link) || b != hashToken(cookie) || strings.Contains(a, link) || strings.Contains(b, cookie) {
		t.Errorf("stored %q and %q", a, b)
	}
}

func TestLoginLinkReplacesUnused(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	first, _ := IssueLocalLink(ctx, st)
	second, _ := IssueLocalLink(ctx, st)
	if ok, _ := ConsumeLocalLink(ctx, st, first); ok {
		t.Error("the replaced link worked")
	}
	if ok, _ := ConsumeLocalLink(ctx, st, second); !ok {
		t.Error("the new link did not work")
	}
}

func TestLocalSessionLifetime(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	token, id, err := CreateLocalSession(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	s, err := FindLocalSession(ctx, st, token)
	if err != nil || s == nil || s.ID != id {
		t.Fatalf("live: %v %v", s, err)
	}
	now := ms(time.Now())
	// A touch moves the idle expiry, never past the absolute one.
	st.DB.Exec(`UPDATE local_sessions SET last_seen_at = ?, absolute_expires_at = ?`, now-2*time.Hour.Milliseconds(), now+time.Hour.Milliseconds())
	if s, _ = FindLocalSession(ctx, st, token); s == nil || s.ExpiresAt > now+time.Hour.Milliseconds() {
		t.Errorf("after a touch: %+v", s)
	}
	st.DB.Exec(`UPDATE local_sessions SET idle_expires_at = ?`, now-1)
	if s, _ = FindLocalSession(ctx, st, token); s != nil {
		t.Error("an idle-expired session worked")
	}
	token, _, _ = CreateLocalSession(ctx, st)
	st.DB.Exec(`UPDATE local_sessions SET absolute_expires_at = ?`, now-1)
	if s, _ = FindLocalSession(ctx, st, token); s != nil {
		t.Error("an absolute-expired session worked")
	}
	if LocalIdleTTL != 12*time.Hour || LocalAbsoluteTTL != 7*24*time.Hour {
		t.Errorf("documented lifetimes changed: %v %v", LocalIdleTTL, LocalAbsoluteTTL)
	}
}

func TestLocalSessionsCappedAndRevoked(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	oldest, _, _ := CreateLocalSession(ctx, st)
	st.DB.Exec(`UPDATE local_sessions SET last_seen_at = 1`)
	var tokens []string
	for range MaxLocalSessions {
		tok, _, err := CreateLocalSession(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	list, err := ListLocalSessions(ctx, st)
	if err != nil || len(list) != MaxLocalSessions {
		t.Fatalf("%d sessions, %v", len(list), err)
	}
	if s, _ := FindLocalSession(ctx, st, oldest); s != nil {
		t.Error("the least recently used session survived")
	}
	if found, _ := RevokeLocalSession(ctx, st, list[0].ID); !found {
		t.Error("revoke found nothing")
	}
	if found, _ := RevokeLocalSession(ctx, st, "no-such-id"); found {
		t.Error("revoke found an unknown id")
	}
	link, _ := IssueLocalLink(ctx, st)
	if n, err := RevokeAllLocal(ctx, st); err != nil || n != MaxLocalSessions-1 {
		t.Errorf("revoke all: %d %v", n, err)
	}
	if ok, _ := ConsumeLocalLink(ctx, st, link); ok {
		t.Error("revoke all left the link")
	}
	if s, _ := FindLocalSession(ctx, st, tokens[1]); s != nil {
		t.Error("revoke all left a session")
	}
}

// The rule without a socket: every way of failing it, one at a time.
func TestLocalRequestRule(t *testing.T) {
	loop := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2273}
	req := func(mod func(*http.Request), local net.Addr) *http.Request {
		r := httptest.NewRequest("GET", "http://127.0.0.1:2273/app", nil)
		r.RemoteAddr = "127.0.0.1:50000"
		if local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		}
		if mod != nil {
			mod(r)
		}
		return r
	}
	if !LocalRequest(req(nil, loop)) {
		t.Fatal("the plain local request should pass")
	}
	if !LocalRequest(req(func(r *http.Request) { r.Host = "localhost:2273" }, loop)) {
		t.Error("localhost should pass")
	}
	if !LocalRequest(req(func(r *http.Request) { r.RemoteAddr, r.Host = "[::1]:50000", "[::1]:2273" }, &net.TCPAddr{IP: net.IPv6loopback, Port: 2273})) {
		t.Error("::1 should pass")
	}
	refused := map[string]*http.Request{
		"remote socket":  req(func(r *http.Request) { r.RemoteAddr = "192.168.1.9:50000" }, loop),
		"listener wide":  req(nil, &net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 2273}),
		"no listener":    req(nil, nil),
		"public host":    req(func(r *http.Request) { r.Host = "localbase.example.com" }, loop),
		"rebinding host": req(func(r *http.Request) { r.Host = "attacker.example:2273" }, loop),
		"another port":   req(func(r *http.Request) { r.Host = "127.0.0.1:9999" }, loop),
		"no port":        req(func(r *http.Request) { r.Host = "127.0.0.1" }, loop),
	}
	for _, h := range proxyHeaders {
		refused[h] = req(func(r *http.Request) { r.Header.Set(h, "1.2.3.4") }, loop)
	}
	for name, r := range refused {
		if LocalRequest(r) {
			t.Errorf("%s passed", name)
		}
	}
}
