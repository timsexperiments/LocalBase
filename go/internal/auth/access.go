package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Browser access configuration: <root>/ui-access.json, the TS
// browser-access-contract format. GitHub OAuth registrations (dropped in Go,
// DESIGN §11) are kept in the file so a rollback finds them, but they offer
// no sign-in.

// Registration is one direct sign-in provider.
type Registration struct {
	Kind string `json:"kind"` // oidc | magic-link | github-oauth
	ID   string `json:"id"`
	Name string `json:"name"`
	// OIDC
	Issuer               string          `json:"issuer,omitempty"`
	ClientID             string          `json:"clientId,omitempty"`
	ClientAuthentication *ClientAuth     `json:"clientAuthentication,omitempty"`
	ClientSecret         string          `json:"clientSecret,omitempty"` // github-oauth only
	raw                  json.RawMessage // github-oauth: preserved as written
}

// ClientAuth is the OIDC token endpoint authentication.
type ClientAuth struct {
	Kind         string `json:"kind"` // none | client-secret-basic
	ClientSecret string `json:"clientSecret,omitempty"`
}

func (r Registration) MarshalJSON() ([]byte, error) {
	if r.raw != nil {
		return r.raw, nil
	}
	type plain Registration
	return json.Marshal(plain(r))
}

// Provider is Cloudflare Access or a set of direct registrations.
type Provider struct {
	Kind          string         `json:"kind"` // cloudflare-access | direct
	TeamDomain    string         `json:"teamDomain,omitempty"`
	Audience      string         `json:"audience,omitempty"`
	Registrations []Registration `json:"registrations,omitempty"`
}

// BrowserAccess is ui-access.json.
type BrowserAccess struct {
	Provider    Provider `json:"provider"`
	Origin      string   `json:"origin"`
	Permissions []string `json:"permissions"`
}

// DefaultBrowserPermissions are granted when no policy is configured.
var DefaultBrowserPermissions = []string{"inference:chat", "models:read"}

var (
	teamDomainRe     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)
	registrationIDRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
)

// ValidOrigin: an exact HTTPS origin, no path, credentials or trailing slash.
func ValidOrigin(o string) bool {
	u, err := url.Parse(o)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && "https://"+u.Host == o
}

func validIssuer(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		!strings.Contains(v, "?") && !strings.Contains(v, "#")
}

func trimmed(s string) bool { return s != "" && strings.TrimSpace(s) == s }

// Validate checks the TS schema rules.
func (b *BrowserAccess) Validate() error {
	if !ValidOrigin(b.Origin) {
		return errors.New("origin: Expected an exact HTTPS origin without path, credentials, or trailing slash.")
	}
	for _, p := range b.Permissions {
		if !contains(Permissions, p) {
			return fmt.Errorf("permissions: unknown permission %s", p)
		}
	}
	switch b.Provider.Kind {
	case "cloudflare-access":
		if !teamDomainRe.MatchString(b.Provider.TeamDomain) || !trimmed(b.Provider.Audience) || len(b.Provider.Registrations) > 0 {
			return errors.New("provider: invalid Cloudflare Access settings")
		}
	case "direct":
		regs := b.Provider.Registrations
		if len(regs) < 1 || len(regs) > 16 {
			return errors.New("provider.registrations: must list 1 to 16 registrations")
		}
		ids, magic := map[string]bool{}, 0
		for _, r := range regs {
			if !registrationIDRe.MatchString(r.ID) || ids[r.ID] {
				return errors.New("Identity provider registration IDs must be unique.")
			}
			ids[r.ID] = true
			if !trimmed(r.Name) || len(r.Name) > 64 {
				return errors.New("registration name is invalid")
			}
			switch r.Kind {
			case "oidc":
				if !validIssuer(r.Issuer) || !trimmed(r.ClientID) || r.ClientAuthentication == nil ||
					(r.ClientAuthentication.Kind != "none" && r.ClientAuthentication.Kind != "client-secret-basic") ||
					(r.ClientAuthentication.Kind == "client-secret-basic" && r.ClientAuthentication.ClientSecret == "") {
					return errors.New("Expected an HTTPS OIDC issuer without credentials, query, or fragment.")
				}
			case "magic-link":
				magic++
			case "github-oauth":
			default:
				return fmt.Errorf("unknown registration kind %s", r.Kind)
			}
		}
		if magic > 1 {
			return errors.New("Only one magic-link registration may be configured.")
		}
	default:
		return errors.New("provider: unknown kind")
	}
	return nil
}

func (r *Registration) UnmarshalJSON(b []byte) error {
	var probe struct{ Kind string }
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	type plain Registration
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode((*plain)(r)); err != nil {
		return err
	}
	if probe.Kind == "github-oauth" {
		r.raw = append(json.RawMessage{}, b...)
	}
	return nil
}

// AccessPath is the browser access config file.
func AccessPath(rootDir string) string { return filepath.Join(rootDir, "ui-access.json") }

// LoadAccess reads ui-access.json; nil when browser access is off.
func LoadAccess(rootDir string) (*BrowserAccess, error) {
	b, err := os.ReadFile(AccessPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("Unable to read browser access configuration.")
	}
	var a BrowserAccess
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&a) != nil || a.Validate() != nil {
		return nil, errors.New("Invalid ui-access.json. Configure browser access again with the LocalBase CLI.")
	}
	a.Permissions = Canonical(a.Permissions)
	return &a, nil
}

// writePrivate atomically writes a 0600 file.
func writePrivate(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + "." + store.UUID() + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SaveAccess validates and writes ui-access.json.
func SaveAccess(rootDir string, a *BrowserAccess) error {
	a.Permissions = Canonical(a.Permissions)
	if err := a.Validate(); err != nil {
		return err
	}
	return writePrivate(AccessPath(rootDir), a)
}

// DisableAccess removes ui-access.json; false if it was absent.
func DisableAccess(rootDir string) (bool, error) {
	err := os.Remove(AccessPath(rootDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// UpsertRegistration replaces or adds a direct registration (a Cloudflare
// provider is replaced by a direct one).
func UpsertRegistration(current *BrowserAccess, reg Registration, origin string, permissions []string) *BrowserAccess {
	var regs []Registration
	if current != nil && current.Provider.Kind == "direct" {
		for _, r := range current.Provider.Registrations {
			if r.ID != reg.ID {
				regs = append(regs, r)
			}
		}
	}
	regs = append(regs, reg)
	sort.SliceStable(regs, func(i, j int) bool { return regs[i].ID < regs[j].ID })
	return &BrowserAccess{Provider: Provider{Kind: "direct", Registrations: regs}, Origin: origin, Permissions: Canonical(permissions)}
}

// RemoveRegistration returns not-found, disabled (none left) or configured.
func RemoveRegistration(current *BrowserAccess, id string) (string, *BrowserAccess) {
	if current == nil || current.Provider.Kind != "direct" {
		return "not-found", nil
	}
	var regs []Registration
	for _, r := range current.Provider.Registrations {
		if r.ID != id {
			regs = append(regs, r)
		}
	}
	if len(regs) == len(current.Provider.Registrations) {
		return "not-found", nil
	}
	if len(regs) == 0 {
		return "disabled", nil
	}
	n := *current
	n.Provider.Registrations = regs
	return "configured", &n
}

// Summary is the config without secrets.
func (b *BrowserAccess) Summary() map[string]any {
	prov := map[string]any{"kind": b.Provider.Kind}
	if b.Provider.Kind == "cloudflare-access" {
		prov["teamDomain"], prov["audience"] = b.Provider.TeamDomain, b.Provider.Audience
	} else {
		regs := []map[string]any{}
		for _, r := range b.Provider.Registrations {
			m := map[string]any{"kind": r.Kind, "id": r.ID, "name": r.Name}
			switch r.Kind {
			case "oidc":
				m["issuer"], m["clientId"], m["clientAuthentication"] = r.Issuer, r.ClientID, r.ClientAuthentication.Kind
			case "github-oauth":
				m["clientId"], m["clientAuthentication"] = r.ClientID, "client-secret"
			}
			regs = append(regs, m)
		}
		prov["registrations"] = regs
	}
	return map[string]any{"provider": prov, "origin": b.Origin, "permissions": b.Permissions}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
