package install

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A server that stays connected and stops sending. The install ends with a retryable
// reason, keeps what arrived, and the next attempt resumes from there.
func TestStalledDownloadFailsAndResumes(t *testing.T) {
	t.Setenv("LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS", "150")
	data := strings.Repeat("x", 2048)
	half := len(data) / 2
	release := make(chan struct{})
	stall := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stall {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, data[half:])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, data[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	partial := filepath.Join(t.TempDir(), "model.partial")
	nop := func(int64) {}

	done := make(chan error, 1)
	go func() {
		done <- download(context.Background(), srv.URL, partial, int64(len(data)), "", nil, nil, nop)
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled download never ended")
	}
	if got := Reason(err); got != ReasonTimeout {
		t.Errorf("reason %q for %v", got, err)
	}
	info, statErr := os.Stat(partial)
	if statErr != nil || info.Size() != int64(half) {
		t.Fatalf("the partial should hold what arrived: %v %v", info, statErr)
	}

	stall = false
	if err := download(context.Background(), srv.URL, partial, int64(len(data)), "", nil, nil, nop); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if b, _ := os.ReadFile(partial); string(b) != data {
		t.Errorf("after resume: %d bytes", len(b))
	}
}
