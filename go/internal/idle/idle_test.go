package idle

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func get(t *testing.T, url string, limit time.Duration) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Do(http.DefaultClient, req, limit)
}

func wantStall(t *testing.T, err error) {
	t.Helper()
	var stalled *Error
	var netErr net.Error
	if !errors.As(err, &stalled) || !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("want a stall that is a net timeout: %v", err)
	}
}

func TestStallAfterSomeBytes(t *testing.T) {
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
	resp, err := get(t, srv.URL, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	began := time.Now()
	got, err := io.ReadAll(resp.Body)
	wantStall(t, err)
	if string(got) != "partial" {
		t.Errorf("bytes before the stall: %q", got)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestStallBeforeTheResponse(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	_, err := get(t, srv.URL, 100*time.Millisecond)
	wantStall(t, err)
}

// A slow download that keeps sending is fine, however long it takes in all.
func TestSlowIsNotStalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 6 {
			io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()
	resp, err := get(t, srv.URL, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || len(got) != 6 {
		t.Errorf("%q %v", got, err)
	}
}

// Time spent handling what arrived is not silence.
func TestTimeBetweenReadsDoesNotCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "abcdef")
	}))
	defer srv.Close()
	resp, err := get(t, srv.URL, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 3)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Errorf("after a pause on this side: %v", err)
	}
}

// A cancel stays a cancel, so an install reports it as interrupted.
func TestCancelIsNotAStall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := Do(http.DefaultClient, req, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want the cancel: %v", err)
	}
}

func TestTimeoutSetting(t *testing.T) {
	if Timeout() != DefaultTimeout {
		t.Errorf("default: %v", Timeout())
	}
	t.Setenv("LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS", "250")
	if Timeout() != 250*time.Millisecond {
		t.Errorf("set: %v", Timeout())
	}
	t.Setenv("LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS", "nonsense")
	if Timeout() != DefaultTimeout {
		t.Errorf("bad value: %v", Timeout())
	}
}
