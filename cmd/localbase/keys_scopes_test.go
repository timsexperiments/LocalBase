package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// --scopes is a comma-separated list: spaces around a scope are dropped, the result is in canonical
// order, and an unknown scope is refused with the permissions it could be.
func TestKeyScopesAreTrimmedAndCanonical(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	code, out := runApp(t, "--root", root, "--json", "keys", "create", "--name", "k", "--scopes", " keys:read , inference:chat")
	if code != 0 {
		t.Fatalf("create: %d %s", code, out)
	}
	var env struct {
		Data struct {
			Key struct {
				Scopes []string `json:"scopes"`
			} `json:"key"`
		} `json:"data"`
	}
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&env); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := env.Data.Key.Scopes; !slices.Equal(got, []string{"inference:chat", "keys:read"}) {
		t.Errorf("scopes %q", got)
	}
	if code, out := runApp(t, "--root", root, "keys", "create", "--name", "k2", "--scopes", "keys:read,keys:everything"); code != 2 || !strings.Contains(out, `scopes: Invalid option: expected one of "inference:chat"|`) {
		t.Errorf("unknown scope: %d %s", code, out)
	}
}
