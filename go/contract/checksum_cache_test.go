package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// After a Go install re-records a file's identity, TS's own verifier still trusts the
// checksum cache, so rolling back does not cost a hash or lose anything.
func TestChecksumCacheStaysValidForTS(t *testing.T) {
	if *implFlag != "both" {
		t.Skip("needs -impl=both")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("needs bun")
	}
	data := []byte(strings.Repeat("model bytes ", 500))
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveBytes(w, r, data) }), false)
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	entry, _ := json.Marshal(map[string]any{
		"modelId": "cache-stt", "displayName": "Cache", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": srv.URL + "/localbase/cache", "repositoryRevision": strings.Repeat("d", 40),
		"artifacts":       []any{map[string]any{"sourcePath": "c.bin", "filename": "c.bin", "expectedSizeBytes": len(data), "sha256": digest, "role": "primary"}},
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture.",
	})
	mustMkdirAll(filepath.Join(e.root, "catalog.d"), 0o700)
	mustWriteFile(filepath.Join(e.root, "catalog.d", "cache-stt.json"), entry, 0o600)
	allowPrivateImports(t, e.root)
	if r := e.cli(nil, "--non-interactive", "models", "install", "cache-stt"); r.code != 0 {
		t.Fatalf("install: %s", r.stderr)
	}
	dir := filepath.Join(e.root, "models", "stt")
	script := fmt.Sprintf(`import { verifyAuthoritativeFile } from "./src/utils/checksum.ts";
console.log(await verifyAuthoritativeFile(%q, { filename: "c.bin", expectedSizeBytes: %d, sha256: %q }, %q));`,
		filepath.Join(dir, "c.bin"), len(data), digest, dir)
	cmd := exec.Command("bun", "-e", script)
	cmd.Dir = repoRoot()
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TS verifier: %v %s", err, out)
	}
	if !strings.Contains(string(out), "cached-identity") {
		t.Errorf("TS did not trust the cache Go wrote: %s", out)
	}
}
