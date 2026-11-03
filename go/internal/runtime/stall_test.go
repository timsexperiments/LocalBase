package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeDownloadStallEnds(t *testing.T) {
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
	done := make(chan error, 1)
	go func() {
		done <- fetchVerified(context.Background(), srv.URL, filepath.Join(t.TempDir(), "a"), 1<<20, "")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stalled download succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled download never ended")
	}
}
