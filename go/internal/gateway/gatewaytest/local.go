package gatewaytest

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Pages are local sign-in's pages as a word each, for tests to tell them apart.
type Pages struct{}

func (Pages) LoginHelp(w http.ResponseWriter, r *http.Request, status int) {
	w.WriteHeader(status)
	if auth.LocalRequest(r) {
		w.Write([]byte("help local"))
		return
	}
	w.Write([]byte("help remote"))
}
func (Pages) LoginUnavailable(w http.ResponseWriter, r *http.Request, status int) {
	w.WriteHeader(status)
	w.Write([]byte("unavailable"))
}
func (Pages) LoginWrongAddress(w http.ResponseWriter, r *http.Request, origin string) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte("wrong address"))
}
func (Pages) LoginBusy(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(503)
	_, _ = w.Write([]byte("busy page"))
}
func (Pages) LoginOnce(w http.ResponseWriter, r *http.Request) { w.Write([]byte("redeem page")) }
func (Pages) LoginExpired(w http.ResponseWriter, r *http.Request, status int) {
	w.WriteHeader(status)
	w.Write([]byte("expired page"))
}
func (Pages) LoginTooMany(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(429)
	w.Write([]byte("too many page"))
}

// LocalClient talks to a real socket, which is what the loopback rule reads.
type LocalClient struct {
	T    *testing.T
	Base string
}

// Send makes a request with the headers in set (Host sets the request's host), without following
// redirects, and returns the response and its body.
func (c LocalClient) Send(method, path, body string, set map[string]string) (*http.Response, string) {
	c.T.Helper()
	req, err := http.NewRequest(method, c.Base+path, strings.NewReader(body))
	if err != nil {
		c.T.Fatal(err)
	}
	for k, v := range set {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		c.T.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Redeem posts a local sign-in link's token as the sign-in page does, with any extra headers.
func (c LocalClient) Redeem(token string, extra map[string]string) (*http.Response, string) {
	c.T.Helper()
	h := map[string]string{"Origin": c.Base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range extra {
		h[k] = v
	}
	return c.Send("POST", auth.LocalOncePath, url.Values{"token": {token}}.Encode(), h)
}

// SessionCookie signs in with a new local link from st and returns the session cookie, checking how
// it is set.
func SessionCookie(t *testing.T, c LocalClient, st *store.Store) string {
	t.Helper()
	token, err := auth.IssueLocalLink(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := c.Redeem(token, nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/app" {
		t.Fatalf("redeem: %d %s", resp.StatusCode, body)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.LocalCookie {
			if !ck.HttpOnly || ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Domain != "" || ck.Path != "/" {
				t.Errorf("cookie: %+v", ck)
			}
			return ck.Name + "=" + ck.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

// LocalUI is the headers the UI sends with a local session's cookie, from origin.
func LocalUI(cookie, origin string) map[string]string {
	return map[string]string{"Cookie": cookie, "X-Localbase-Ui": "1", "Origin": origin, "Sec-Fetch-Site": "same-origin"}
}

// MustLocalRequest is a GET of /app with the cookie and the headers in set, not sent.
func MustLocalRequest(t *testing.T, c LocalClient, ck string, set map[string]string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", c.Base+"/app", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Cookie", ck)
	for k, v := range set {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	return r
}
