package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestEnsureBinaryInstallsPinnedRelease drives the managed installer offline:
// a pinned archive and two dictionary sources served locally, checked
// against their recorded sizes and checksums.
func TestEnsureBinaryInstallsPinnedRelease(t *testing.T) {
	archive := tarGz(t, map[string]string{"pkg/crispasr": "#!/bin/sh\n", "pkg/libextra.dylib": "lib", "../escape": "x"})
	gold := []byte(`{"b":"gold","10":"x","a":["g"]}`)
	silver := []byte(`{"a":"silver","c":"s","2":1.50}`)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gold.json"), gold, 0o600)
	os.WriteFile(filepath.Join(dir, "silver.json"), silver, 0o600)
	merged, err := MergeMisaki(filepath.Join(dir, "gold.json"), filepath.Join(dir, "silver.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"2":1.5,"10":"x","b":"gold","a":["g"],"c":"s"}` + "\n"; string(merged) != want {
		t.Fatalf("merged %q, want %q", merged, want)
	}
	served := map[string][]byte{"/a.tar.gz": archive, "/gold.json": gold, "/silver.json": silver}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if b, ok := served[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	oldURL, oldManifest := fetchURL, goManifestJSON
	defer func() { fetchURL, goManifestJSON = oldURL, oldManifest }()
	fetchURL = func(u string) string { return srv.URL + u[strings.LastIndex(u, "/"):] }
	manifest := func(archiveSHA string) []byte {
		arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
		b, _ := json.Marshal(map[string]any{"targets": []any{map[string]any{"platform": goruntime.GOOS, "architecture": arch,
			"runtimes": map[string]any{"crispasr": map[string]any{"tag": "v0", "assetName": "a.tar.gz",
				"url": "https://example.invalid/a.tar.gz", "expectedSizeBytes": len(archive), "sha256": archiveSHA,
				"format": "tar.gz", "stripComponents": 1,
				"supportFiles": []any{map[string]any{"filename": misakiDict, "sha256": hexSum(merged), "derivation": "misaki-gold-first-union",
					"sources": []any{
						map[string]any{"url": "https://example.invalid/gold.json", "expectedSizeBytes": len(gold), "sha256": hexSum(gold)},
						map[string]any{"url": "https://example.invalid/silver.json", "expectedSizeBytes": len(silver), "sha256": hexSum(silver)},
					}}}}}}}})
		return b
	}

	// A wrong archive checksum installs nothing.
	goManifestJSON = manifest(strings.Repeat("0", 64))
	runtimes := filepath.Join(t.TempDir(), "runtimes")
	if _, err := EnsureBinary(context.Background(), runtimes, "crispasr"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(runtimes, "crispasr")); len(entries) != 0 {
		t.Fatalf("left files behind: %v", entries)
	}

	goManifestJSON = manifest(hexSum(archive))
	bin, err := EnsureBinary(context.Background(), runtimes, "crispasr")
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(runtimes, "crispasr", hexSum(archive))
	if bin != filepath.Join(pkg, "crispasr") || !Executable(bin) {
		t.Fatalf("binary %s", bin)
	}
	if !fileExists(filepath.Join(pkg, "libextra.dylib")) || fileExists(filepath.Join(runtimes, "crispasr", "escape")) {
		t.Fatal("archive extraction: missing library or escaped entry")
	}
	if got, _ := os.ReadFile(SupportPath(runtimes, "crispasr", misakiDict)); !bytes.Equal(got, merged) {
		t.Fatalf("dictionary %q", got)
	}
	var receipt map[string]any
	b, _ := os.ReadFile(filepath.Join(pkg, ".managed-binaries.json"))
	if json.Unmarshal(b, &receipt) != nil || receipt["runtimes"].(map[string]any)["crispasr"] == nil {
		t.Fatalf("receipt %s", b)
	}

	// Installed: no network.
	n := hits.Load()
	if _, err := EnsureBinary(context.Background(), runtimes, "crispasr"); err != nil || hits.Load() != n {
		t.Fatalf("reinstalled an installed runtime: %v, %d requests", err, hits.Load()-n)
	}
	// A damaged dictionary is rebuilt, not run.
	os.WriteFile(filepath.Join(pkg, misakiDict), []byte("{}"), 0o600)
	if _, err := EnsureBinary(context.Background(), runtimes, "crispasr"); err != nil || hits.Load() == n {
		t.Fatalf("damaged dictionary: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(pkg, misakiDict)); !bytes.Equal(got, merged) {
		t.Fatal("dictionary not rebuilt")
	}
}
