package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

// Every settings writer: a planted link fails that write with the clear error and
// leaves its target alone, without stopping anything else.
func TestSettingsWritersRefuseAPlantedLink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "victim")
	content := []byte("someone's own file\n")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	access := &auth.BrowserAccess{Provider: auth.Provider{Kind: "cloudflare-access", TeamDomain: "team.cloudflareaccess.com", Audience: "aud"},
		Origin: "https://lb.example.com", Permissions: []string{"models:read"}}
	for name, c := range map[string]struct {
		rel   string
		write func(root string) error
	}{
		"ui-access.json": {"ui-access.json", func(root string) error { return auth.SaveAccess(root, access) }},
		"auth profiles": {filepath.Join("credentials", "auth-profiles.json"), func(root string) error {
			return acquire.SaveProfile(root, acquire.Profile{ID: "vendor", Origins: []string{"https://cdn.example.com"}, Headers: map[string]string{"X-Key": "secret"}})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, c.rel)
			os.MkdirAll(filepath.Dir(path), 0o700)
			os.Symlink(target, path)
			var unsafe *safefile.UnsafeFileError
			if err := c.write(root); !errors.As(err, &unsafe) {
				t.Errorf("want a refusal: %v", err)
			}
			if got, _ := os.ReadFile(target); string(got) != string(content) {
				t.Errorf("the target changed: %q", got)
			}
			// A normal root writes, private.
			clean := t.TempDir()
			if err := c.write(clean); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(clean, c.rel))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%v %v", info, err)
			}
		})
	}
}
