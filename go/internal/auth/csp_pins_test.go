package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The sign-in pages take their styles from the app's stylesheet and allow nothing inline.
// Their script is pinned by hash elsewhere; this pins the rest of the policy.
func TestSignInPagesAllowNoInlineStyles(t *testing.T) {
	f := newFakeIdP(t)
	reg := func(id string) Registration {
		return Registration{Kind: "oidc", ID: id, Name: id, Issuer: f.srv.URL, ClientID: "cid",
			ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	}
	b := browserFor(t, testStore(t), reg("one"), reg("two"))
	b.Client = f.srv.Client()
	pages := map[string]func(*httptest.ResponseRecorder){
		"provider chooser": func(w *httptest.ResponseRecorder) { b.StartLogin(w, get("/app/login")) },
		"unknown provider": func(w *httptest.ResponseRecorder) {
			r := get("/app/login?provider=nope")
			r.Header.Set("Accept", "text/html")
			b.StartLogin(w, r)
		},
		"magic link page": func(w *httptest.ResponseRecorder) { b.MagicLinkPage(w) },
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
		if strings.Contains(w.Body.String(), "<style") || strings.Contains(w.Body.String(), ` style="`) {
			t.Errorf("%s has inline styles: %s", name, w.Body.String())
		}
	}
}
