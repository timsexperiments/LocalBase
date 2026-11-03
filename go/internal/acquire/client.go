package acquire

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/netguard"
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
func newClient(profile *Profile, hfToken string, hfExtra []string, allowPrivate bool) *http.Client {
	hfOrig, _ := Origin(HFEndpoint())
	base := http.DefaultTransport.(*http.Transport).Clone()
	if pool := extraRoots(); pool != nil {
		base.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	base.ResponseHeaderTimeout = 60 * time.Second
	guarded := importPolicy(allowPrivate).Transport(base)
	return &http.Client{
		Transport:     &scopedTransport{base: guarded, profile: profile, hfToken: hfToken, hfOrigs: append([]string{hfOrig}, hfExtra...)},
		CheckRedirect: redirectPolicy,
	}
}

// Stable codes for a source the policy refuses (DESIGN §31.2).
const (
	CodePrivateAddress = "import_private_address"
	CodeAddressRefused = "import_address_refused"
	CodeLocalPath      = "import_local_path"
)

// RefusedError is a source the policy refuses. Its Message is written for whoever
// asked, who may be on a phone and not the person running this computer, so through
// the API and the pages it names no setting and nothing about the address.
type RefusedError struct{ Code, Message string }

func (e *RefusedError) Error() string { return e.Message }

const (
	privateAddressMessage = "This address is on your local network. Ask whoever runs this computer to allow it."
	addressRefusedMessage = "LocalBase does not download from this kind of address."
	localPathMessage      = "That is not a web address or a Hugging Face repository, and importing a file from this computer is off here. Ask whoever runs this computer to allow it."
	// privateAddressOnHost is for the person at this computer, who can allow it.
	privateAddressOnHost = "This address is on your local network or this computer. To allow it, add --allow-private to this import, or run `local-base configure --allow-private-imports` to allow it for every import."
)

// refusal turns a refused connection into what the person asking is told.
func refusal(err error, onHost bool) error {
	var blocked *netguard.BlockedError
	if !errors.As(err, &blocked) {
		return err
	}
	switch {
	case blocked.Class == netguard.Never:
		return &RefusedError{CodeAddressRefused, addressRefusedMessage}
	case onHost:
		return &RefusedError{CodePrivateAddress, privateAddressOnHost}
	}
	return &RefusedError{CodePrivateAddress, privateAddressMessage}
}

// importPolicy keeps downloads to public addresses unless opted in. HF_ENDPOINT is
// named by the person running LocalBase, so a private mirror there is allowed.
func importPolicy(allowPrivate bool) netguard.Policy {
	p := netguard.Policy{AllowPrivate: allowPrivate}
	if u, err := url.Parse(HFEndpoint()); err == nil && u.Hostname() != "" {
		p.AllowHosts = []string{u.Hostname()}
	}
	return p
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
	var blocked *netguard.BlockedError
	if errors.As(err, &blocked) {
		return blocked // names a host, and callers can tell it apart
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = fmt.Errorf("%s %s: %v", ue.Op, RedactURL(ue.URL), ue.Err)
	}
	if red != nil {
		return errors.New(red.Text(err.Error()))
	}
	return err
}

// extraRoots adds the certificates in LOCALBASE_DOWNLOAD_CA_FILE (PEM) to
// the system roots for downloads, for networks with their own TLS CA (and
// tests). Validation stays on; this only adds trust anchors.
func extraRoots() *x509.CertPool {
	path := os.Getenv("LOCALBASE_DOWNLOAD_CA_FILE")
	if path == "" {
		return nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil
	}
	return pool
}
