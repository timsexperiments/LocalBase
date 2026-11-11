package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// An install that is downloading holds only its own family, and someone waiting for that family
// stops waiting when told to (review 04-M3). Before, one process-wide mutex was held across every
// download: another family's install waited for it, and so did a switch being ended by Close.
func TestAnInstallHoldsOnlyItsFamilyAndWaitingCanBeEnded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/slow.tar.gz") {
			select { // a download that doesn't finish until the test lets it go
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	oldURL, oldManifest := fetchURL, goManifestJSON
	defer func() { fetchURL, goManifestJSON = oldURL, oldManifest }()
	fetchURL = func(u string) string { return srv.URL + u[strings.LastIndex(u, "/"):] }
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
	artifact := func(asset string) map[string]any {
		return map[string]any{"tag": "v0", "assetName": asset, "url": "https://example.invalid/" + asset,
			"expectedSizeBytes": 10, "sha256": strings.Repeat("0", 64), "format": "tar.gz", "stripComponents": 1}
	}
	goManifestJSON, _ = json.Marshal(map[string]any{"targets": []any{map[string]any{"platform": goruntime.GOOS, "architecture": arch,
		"runtimes": map[string]any{"crispasr": artifact("slow.tar.gz"), "tts-server": artifact("other.tar.gz")}}}})
	runtimes := filepath.Join(t.TempDir(), "runtimes")

	first := make(chan error, 1)
	installed := make(chan struct{})
	go func() {
		defer close(installed)
		_, err := EnsureBinary(context.Background(), runtimes, "crispasr")
		first <- err
	}()
	// The first install outlives the checks below. On the way out the slow download is let go and
	// the test waits for that install to end, so it has removed its .install-* directory before
	// fetchURL and goManifestJSON go back and t.TempDir's cleanup runs. Without the wait, the
	// cleanup raced the install's own (unlinkat .install-*: directory not empty).
	defer func() {
		close(release)
		<-installed
	}()
	time.Sleep(300 * time.Millisecond) // the first install is downloading

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := EnsureBinary(ctx, runtimes, "crispasr")
	if took := time.Since(began); took > 1500*time.Millisecond || err == nil {
		t.Errorf("waiting for the same family: %v after %v; it should end with its context", err, took)
	}

	began = time.Now()
	_, _ = EnsureBinary(context.Background(), runtimes, "tts-server") // not served: fails at once
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("another family waited %v for an unrelated download", took)
	}
	select {
	case err := <-first:
		t.Fatalf("the slow install ended early: %v", err)
	default:
	}
}
