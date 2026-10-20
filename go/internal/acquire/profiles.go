// Package acquire resolves and downloads model artifacts from Hugging Face,
// direct HTTP(S) URLs and local files into staging, validates them, and
// publishes them. Acquisition is independent of whether any backend can run
// the result.
package acquire

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
)

// Origin returns the scheme://host[:port] of an http(s) URL.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("not an http(s) URL")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// Validate checks a profile.
func (p *Profile) Validate() error {
	if !profileIDRe.MatchString(p.ID) {
		return fmt.Errorf("profile id %q must be lowercase letters, digits, '.', '_' or '-'", p.ID)
	}
	if len(p.Origins) == 0 {
		return errors.New("a profile needs at least one origin it may be sent to")
	}
	for i, o := range p.Origins {
		norm, err := Origin(o)
		if err != nil || strings.TrimSuffix(strings.ToLower(o), "/") != norm {
			return fmt.Errorf("origin %q must be exactly scheme://host[:port]", o)
		}
		p.Origins[i] = norm
	}
	if len(p.Headers) == 0 {
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

// Profiles is the profile file.
type Profiles struct {
	Profiles []Profile `json:"profiles"`
}

// LoadProfiles reads the profiles; a missing file is empty.
func LoadProfiles(rootDir string) (*Profiles, error) {
	b, err := os.ReadFile(profilesPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return &Profiles{}, nil
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
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ps, "", "  ")
	tmp := profilesPath(rootDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, profilesPath(rootDir))
}

// SaveProfile adds or replaces a profile.
func SaveProfile(rootDir string, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ps, err := LoadProfiles(rootDir)
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
	ps, err := LoadProfiles(rootDir)
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
