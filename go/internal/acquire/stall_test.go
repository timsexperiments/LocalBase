package acquire

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An import from a server that stops sending ends, keeping what arrived.
func TestImportDownloadStallEnds(t *testing.T) {
	t.Setenv("LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS", "150")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	partial := filepath.Join(t.TempDir(), "a.partial")
	done := make(chan error, 1)
	go func() {
		done <- download(context.Background(), srv.Client(), srv.URL, partial, 1<<20, nil, newBudget(filepath.Dir(partial), 1<<20), func(int64) {})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stalled download succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled download never ended")
	}
	if b, _ := os.ReadFile(partial); string(b) != "partial" {
		t.Errorf("partial: %q", b)
	}
}
