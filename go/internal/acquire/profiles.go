// Package acquire resolves and downloads model artifacts from Hugging Face,
// direct HTTP(S) URLs and local files into staging, validates them, and
// publishes them. Acquisition is independent of whether any backend can run
// the result.
package acquire

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

// Auth profiles are LocalBase's private credential storage for download
// headers: <root>/credentials/auth-profiles.json, mode 0600 in a 0700
// directory. Model records reference a profile by id; header values never
// leave this file except as request headers to the profile's origins.

// Profile is a named set of request headers scoped to origins.
type Profile struct {
	ID      string            `json:"id"`
	Origins []string          `json:"origins"` // exact scheme://host[:port]
	Headers map[string]string `json:"headers"` // name -> secret value
}

// PublicProfile is a profile without its secret values.
type PublicProfile struct {
	ID          string   `json:"id"`
	Origins     []string `json:"origins"`
	HeaderNames []string `json:"headerNames"`
}

// Public strips the values.
func (p *Profile) Public() PublicProfile {
	names := make([]string, 0, len(p.Headers))
	for k := range p.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	return PublicProfile{ID: p.ID, Origins: p.Origins, HeaderNames: names}
}

// Reserved headers belong to the downloader: they frame the transfer and
// cannot be set by a profile.
var reserved = map[string]bool{
	"Range": true, "If-Range": true, "Host": true, "Content-Length": true, "Transfer-Encoding": true,
	"Connection": true, "Accept-Encoding": true, "Te": true, "Trailer": true, "Upgrade": true,
	"Proxy-Authorization": true, "Proxy-Connection": true, "Keep-Alive": true,
}

var (
	profileIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")
	// lowerASCIIHost is a host of lower-case ASCII labels, none empty, so no letter from another alphabet, and no
	// upper case, can make a host look like one it isn't.
	lowerASCIIHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// hfOwnOrigin says whether the huggingface profile may name an origin: the origin HF_ENDPOINT names, or one of
// Hugging Face's own, written as https:// and the host alone. The parsed host is compared whole: huggingface.co
// or hf.co, or a subdomain of either at any depth, so nothing before or after it, no user or port, and no other
// case or alphabet passes. hf.space isn't one of them, as anyone can run a Space there.
func hfOwnOrigin(origin, endpoint string) bool {
	origin = strings.TrimSuffix(origin, "/")
	if endpoint != "" && origin == endpoint {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || origin != "https://"+u.Host || !lowerASCIIHost.MatchString(u.Host) {
		return false
	}
	for _, own := range []string{"huggingface.co", "hf.co"} {
		if u.Host == own || strings.HasSuffix(u.Host, "."+own) {
			return true
		}
	}
	return false
}

// hfEndpointOrigin is the origin HF_ENDPOINT names, from the environment and never from a profile.
func hfEndpointOrigin() string {
	o, err := Origin(HFEndpoint())
	if err != nil {
		return ""
	}
	return o
}

// shown keeps a value from a file short in a message.
func shown(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

// Origin returns the scheme://host[:port] of an http(s) URL.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("not an http(s) URL")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// Validate checks a profile, with HF_ENDPOINT as the environment says now.
func (p *Profile) Validate() error { return p.validate(hfEndpointOrigin()) }

// validate checks a profile against the origin HF_ENDPOINT named when the profiles were read.
func (p *Profile) validate(endpoint string) error {
	if !profileIDRe.MatchString(p.ID) {
		return fmt.Errorf("profile id %q must be lowercase letters, digits, '.', '_' or '-'", p.ID)
	}
	if len(p.Origins) == 0 {
		return errors.New("a profile needs at least one origin it may be sent to")
	}
	for i, o := range p.Origins {
		if p.ID == HFProfileID && !hfOwnOrigin(o, endpoint) {
			return fmt.Errorf("origin %q isn't Hugging Face's own: the huggingface profile can only name huggingface.co, hf.co, their subdomains, or the origin HF_ENDPOINT names", o)
		}
		norm, err := Origin(o)
		if err != nil || strings.TrimSuffix(strings.ToLower(o), "/") != norm {
			return fmt.Errorf("origin %q must be exactly scheme://host[:port]", o)
		}
		p.Origins[i] = norm
	}
	if len(p.Headers) == 0 && p.ID != HFProfileID {
		return errors.New("a profile needs at least one header")
	}
	for name, value := range p.Headers {
		canon := http.CanonicalHeaderKey(name)
		if !headerNameRe.MatchString(name) {
			return fmt.Errorf("header name %q is not a valid HTTP header name", name)
		}
		if reserved[canon] {
			return fmt.Errorf("header %s is controlled by the downloader", canon)
		}
		if value == "" || strings.ContainsAny(value, "\r\n\x00") || len(value) > 16384 {
			return fmt.Errorf("header %s needs a single-line value (at most 16 KiB)", canon)
		}
	}
	return nil
}

// Allows reports whether the profile may be sent to origin.
func (p *Profile) Allows(origin string) bool {
	for _, o := range p.Origins {
		if o == origin {
			return true
		}
	}
	return false
}

func profilesPath(rootDir string) string {
	return filepath.Join(rootDir, "credentials", "auth-profiles.json")
}

// Profiles is the profile file. Loaded with LoadProfiles, it holds only profiles that passed their checks.
type Profiles struct {
	Profiles []Profile `json:"profiles"`
	// Warnings say what the huggingface profile named and can't, which was left out.
	Warnings []string `json:"-"`
	invalid  []invalidProfile
}

// invalidProfile is a profile kept out of use, and why, in Validate's words, which quote what they show.
type invalidProfile struct{ id, why string }

// LoadProfiles reads the profiles for use. Each passes Validate, against HF_ENDPOINT as it is now, read once for
// all of them. An origin the huggingface profile can't name is left out, with a warning, and the rest of that
// profile still works. Any other profile that fails is kept out of use, and naming it is an error: see Invalid.
// A missing file is empty.
func LoadProfiles(rootDir string) (*Profiles, error) {
	raw, err := readProfiles(rootDir)
	if err != nil {
		return nil, err
	}
	endpoint := hfEndpointOrigin()
	out := &Profiles{}
	for _, p := range raw.Profiles {
		if p.ID == HFProfileID {
			var kept []string
			for _, o := range p.Origins {
				if hfOwnOrigin(o, endpoint) {
					kept = append(kept, o)
					continue
				}
				out.Warnings = append(out.Warnings, fmt.Sprintf("The huggingface download profile names %q, which isn't Hugging Face's own, so the Hugging Face token isn't sent there.", shown(o, 200)))
			}
			if p.Origins = kept; len(kept) == 0 {
				continue // nothing left to add: the token goes to HF_ENDPOINT's origin alone
			}
		}
		if err := p.validate(endpoint); err != nil {
			out.invalid = append(out.invalid, invalidProfile{p.ID, shown(err.Error(), 300)})
			continue
		}
		out.Profiles = append(out.Profiles, p)
	}
	return out, nil
}

// Invalid says why the profile with this id is kept out of use, or nil if it isn't.
func (ps *Profiles) Invalid(id string) error {
	for _, p := range ps.invalid {
		if p.id == id {
			return fmt.Errorf("download profile %q is not valid: %s", shown(p.id, 64), p.why)
		}
	}
	return nil
}

// Problems are every profile kept out of use, as Invalid says them.
func (ps *Profiles) Problems() []error {
	var out []error
	for _, p := range ps.invalid {
		out = append(out, ps.Invalid(p.id))
	}
	return out
}

// FirstInvalid is the first profile kept out of use, as Invalid says it, or nil.
func (ps *Profiles) FirstInvalid() error {
	if len(ps.invalid) == 0 {
		return nil
	}
	return ps.Invalid(ps.invalid[0].id)
}

// readProfiles reads the file as written, for the commands that change it, so a profile that fails its checks
// can still be replaced or removed. A missing file is empty.
func readProfiles(rootDir string) (*Profiles, error) {
	b, err := safefile.ReadOwnedRegular(profilesPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return &Profiles{}, nil
	}
	var unsafe *safefile.UnsafeFileError
	if errors.As(err, &unsafe) {
		return nil, err
	}
	if err != nil {
		return nil, errors.New("unable to read the auth profiles")
	}
	var ps Profiles
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, errors.New("the auth profile file is not valid JSON")
	}
	return &ps, nil
}

// Get returns a profile or nil.
func (ps *Profiles) Get(id string) *Profile {
	for i := range ps.Profiles {
		if ps.Profiles[i].ID == id {
			return &ps.Profiles[i]
		}
	}
	return nil
}

func saveProfiles(rootDir string, ps *Profiles) error {
	dir := filepath.Dir(profilesPath(rootDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its execute bit
		return err
	}
	b, _ := json.MarshalIndent(ps, "", "  ")
	return safefile.WritePrivateChecked(profilesPath(rootDir), b)
}

// SaveProfile adds or replaces a profile.
func SaveProfile(rootDir string, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ps, err := readProfiles(rootDir)
	if err != nil {
		return err
	}
	if cur := ps.Get(p.ID); cur != nil {
		*cur = p
	} else {
		ps.Profiles = append(ps.Profiles, p)
	}
	return saveProfiles(rootDir, ps)
}

// RemoveProfile deletes a profile; false when it did not exist.
func RemoveProfile(rootDir, id string) (bool, error) {
	ps, err := readProfiles(rootDir)
	if err != nil {
		return false, err
	}
	for i, p := range ps.Profiles {
		if p.ID == id {
			ps.Profiles = append(ps.Profiles[:i], ps.Profiles[i+1:]...)
			return true, saveProfiles(rootDir, ps)
		}
	}
	return false, nil
}

// RedactURL removes credentials from a URL for display: user info, every
// query value (signed URLs carry their signature there) and the fragment.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable URL]"
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, url.QueryEscape(k)+"=REDACTED")
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	return u.String()
}

// Redactor removes known secret values from text.
type Redactor struct{ secrets []string }

// Add registers secret values (empty and very short values are ignored to
// avoid mangling text).
func (r *Redactor) Add(values ...string) {
	for _, v := range values {
		if len(v) >= 4 {
			r.secrets = append(r.secrets, v)
		}
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Text returns s with every secret replaced.
func (r *Redactor) Text(s string) string {
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

// HFProfileID names the profile whose origins may also receive the Hugging
// Face token: Hugging Face's own hosts, such as its CDN cas-bridge.xethub.hf.co.
// Without it the token goes to the Hugging Face endpoint only, and CDN
// downloads are anonymous, as signed CDN URLs need no token.
const HFProfileID = "huggingface"

// hfTokenOrigins are the origins the huggingface profile adds for the token, after its checks. What it names and
// can't is logged, to log, or to the default logger when that's nil.
func hfTokenOrigins(rootDir string, log *slog.Logger) []string {
	if rootDir == "" {
		return nil
	}
	ps, err := LoadProfiles(rootDir)
	if err != nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	for _, w := range ps.Warnings {
		log.Warn(w, "event", "acquire.profile-origin-ignored")
	}
	if p := ps.Get(HFProfileID); p != nil {
		return p.Origins
	}
	return nil
}
