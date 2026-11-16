package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Every page of local sign-in, including the one for another host name: styles and
// fonts from this origin only, no inline styles, and no inline script beyond a hash.
func TestLocalSignInPagesCSP(t *testing.T) {
	u := shellUI(t, nil, "")
	pages := map[string]func(w *httptest.ResponseRecorder){
		"help": func(w *httptest.ResponseRecorder) {
			u.LoginHelp(w, httptest.NewRequest("GET", "http://127.0.0.1/app/login", nil), 401)
		},
		"once": func(w *httptest.ResponseRecorder) {
			u.LoginOnce(w, httptest.NewRequest("GET", "http://127.0.0.1/app/login/once", nil))
		},
		"expired": func(w *httptest.ResponseRecorder) {
			u.LoginExpired(w, httptest.NewRequest("GET", "http://127.0.0.1/app/login/once", nil), 410)
		},
		"too many": func(w *httptest.ResponseRecorder) {
			u.LoginTooMany(w, httptest.NewRequest("GET", "http://127.0.0.1/app/login/once", nil))
		},
		"unavailable": func(w *httptest.ResponseRecorder) {
			u.LoginUnavailable(w, httptest.NewRequest("GET", "http://127.0.0.1/app/login", nil), 503)
		},
		"wrong address": func(w *httptest.ResponseRecorder) {
			u.LoginWrongAddress(w, httptest.NewRequest("GET", "http://other.test/app/login", nil), "https://sign-in.example")
		},
	}
	for name, render := range pages {
		w := httptest.NewRecorder()
		render(w)
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "style-src 'self'", "font-src 'self'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: no %q in %q", name, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: %q", name, csp)
		}
		body := w.Body.String()
		if strings.Contains(body, "<style") || strings.Contains(body, ` style="`) {
			t.Errorf("%s has inline styles", name)
		}
	}
}
