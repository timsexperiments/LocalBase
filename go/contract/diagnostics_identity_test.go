package contract

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real gateway, a real key used for a real request, and the bundle made afterwards.
func TestABundleOfARealGatewaySaysNothingOfWhoItCameFrom(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	created := e.json("keys", "create", "--name", "planted-key-name", "--scopes", "keys:manage,keys:read")
	secret, _ := created["secret"].(string)
	key, _ := created["key"].(map[string]any)
	id, _ := key["id"].(string)
	if secret == "" || id == "" {
		t.Fatalf("no key: %v", created)
	}
	e.serve()
	defer e.stop()
	req, _ := http.NewRequest("GET", e.base+"/_localbase/api-keys", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("listing keys: %d", resp.StatusCode)
	}
	e.stop()
	out := filepath.Join(tmp, "bundle.zip")
	if r := e.cli(nil, "--json", "diagnostics", "--output", out); r.code != 0 {
		t.Fatalf("diagnostics: %s %s", r.stdout, r.stderr)
	}
	z, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatal(host, err)
	}
	var events string
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		for what, value := range map[string]string{"the root": e.root, "the key's name": "planted-key-name", "the key's id": id, "this computer's name": host} {
			if strings.Contains(string(b), value) {
				t.Errorf("%s holds %s", f.Name, what)
			}
		}
		if f.Name == "logs/events.jsonl" {
			events = string(b)
		}
	}
	// It did see the request, so that's not why they're missing.
	if !strings.Contains(events, "auth_source") {
		t.Errorf("the bundle has no request in it: %.400s", events)
	}
}
