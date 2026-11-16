package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

const testCSRFSession = "test-session-0123456789abcdefghijklmnop"

// withCSRF gives a request the session cookie and its token, as the page does.
func withCSRF(u *UI, req *http.Request) {
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: testCSRFSession})
	req.Header.Set(csrfHeader, u.csrfFor(testCSRFSession))
}

// State-changing htmx routes need the same origin and the session's token:
// a cross-site form post, a post without the token, and a token from another
// session are all refused; the page hands out a token bound to its cookie.
func TestCSRF(t *testing.T) {
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: []string{"models:read", "models:manage"}}
	api := &fakeTokenAPI{}
	u := New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: api.call, RootDir: t.TempDir()})

	page := get(u, "/app/settings")
	var session string
	for _, c := range page.Result().Cookies() {
		if c.Name == csrfCookie {
			session = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/app" {
				t.Errorf("cookie attributes: %+v", c)
			}
		}
	}
	if session == "" || !strings.Contains(page.Body.String(), `"X-CSRF-Token": "`+u.csrfFor(session)+`"`) {
		t.Fatal("the page did not get a session cookie and its token")
	}

	post := func(edit func(*http.Request)) int {
		req := httptest.NewRequest("POST", "/app/settings/hf-token", strings.NewReader(url.Values{"action": {"clear"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Localbase-Ui", "1")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: session})
		req.Header.Set(csrfHeader, u.csrfFor(session))
		edit(req)
		w := httptest.NewRecorder()
		u.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(func(*http.Request) {}); code != 200 {
		t.Errorf("same-origin with the token: %d", code)
	}
	// A cross-site form: no custom headers, the browser's cross-site markers.
	if code := post(func(r *http.Request) {
		r.Header.Del("HX-Request")
		r.Header.Del("X-Localbase-Ui")
		r.Header.Del(csrfHeader)
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}); code != http.StatusForbidden {
		t.Errorf("cross-site form post: %d", code)
	}
	if code := post(func(r *http.Request) { r.Header.Del(csrfHeader) }); code != http.StatusForbidden {
		t.Errorf("without the token: %d", code)
	}
	if code := post(func(r *http.Request) { r.Header.Set(csrfHeader, u.csrfFor("another-session-0123456789abcdefghij")) }); code != http.StatusForbidden {
		t.Errorf("another session's token: %d", code)
	}
	if len(api.sent) != 1 {
		t.Errorf("refused posts reached the API: %d calls", len(api.sent))
	}
}
