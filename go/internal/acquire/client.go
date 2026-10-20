package acquire

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// HFEndpoint is the Hugging Face base URL (HF_ENDPOINT overrides it).
func HFEndpoint() string {
	if v := strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/"); v != "" {
		return v
	}
	return "https://huggingface.co"
}

// scopedTransport adds credentials per request, by that request's own
// origin. Credentials are never set on the request the caller builds, so an
// HTTP redirect to another origin cannot carry them: each hop gets only what
// its origin is entitled to. TLS certificate validation stays on.
type scopedTransport struct {
	base    http.RoundTripper
	profile *Profile
	hfToken string
	hfOrigs []string // origins the Hugging Face token may reach
}

func (t *scopedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin := req.URL.Scheme + "://" + strings.ToLower(req.URL.Host)
	r := req.Clone(req.Context())
	// Remove anything a redirect could have copied over.
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	if t.profile != nil {
		for name := range t.profile.Headers {
			r.Header.Del(name)
		}
		if t.profile.Allows(origin) {
			for name, value := range t.profile.Headers {
				r.Header.Set(name, value)
			}
		}
	}
	if t.hfToken != "" && slices.Contains(t.hfOrigs, origin) && (t.profile == nil || t.profile.Headers["Authorization"] == "") {
		r.Header.Set("Authorization", "Bearer "+t.hfToken)
	}
	return t.base.RoundTrip(r)
}

// newClient returns a client that sends profile headers only to the
// profile's origins, and the Hugging Face token only to the Hugging Face
// endpoint's origin plus any origins listed in a "huggingface" auth profile
// (for example a CDN the person chose to trust with the token). Anonymous
// downloads from CDNs work without it.
func newClient(profile *Profile, hfToken string, hfExtra []string) *http.Client {
	hfOrig, _ := Origin(HFEndpoint())
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{
		Transport:     &scopedTransport{base: base, profile: profile, hfToken: hfToken, hfOrigs: append([]string{hfOrig}, hfExtra...)},
		CheckRedirect: redirectPolicy,
	}
}

// ErrDowngrade: an HTTPS download was redirected to plain HTTP. It is refused,
// not offered for consent: the source chose HTTPS, and a downgrade in the
// middle of a transfer is how a man in the middle would look.
var ErrDowngrade = errors.New("refusing a redirect from HTTPS to plain HTTP")

// redirectPolicy limits redirects and refuses HTTPS to HTTP downgrades
// anywhere in the chain.
func redirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	for _, v := range via {
		if v.URL.Scheme == "https" && req.URL.Scheme != "https" {
			return ErrDowngrade
		}
	}
	return nil
}

// redactErr turns transport errors into text without URLs' secrets: a
// url.Error repeats the full URL, including signed query values.
func redactErr(err error, red *Redactor) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = fmt.Errorf("%s %s: %v", ue.Op, RedactURL(ue.URL), ue.Err)
	}
	if red != nil {
		return errors.New(red.Text(err.Error()))
	}
	return err
}
