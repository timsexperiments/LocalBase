package library

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A launch that installs what its model needs downloads under the launch's own context. When the launch
// is cancelled (the client went away, or the gateway is stopping), the download stops with it, instead
// of running on for as long as a model takes to arrive.
func TestALaunchsDownloadStopsWithTheLaunch(t *testing.T) {
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		// Holds the download open, sending nothing, until the client goes away. Bounded, so a test that
		// never cancels it ends.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	l := newTestLibrary(t)
	l.InstallMissing = true
	// The one built-in model with a variant other than "default", chosen by hand so its files are missing.
	const model, backend, variant = "kokoro-82m-v1.0-q8", "crispasr-cpu", "crispasr-f16"
	if _, err := l.SetBackend(context.Background(), model, backend, variant); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := l.GateFor("speech")(ctx, model, true)
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the launch ended before downloading: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never started its download")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled launch reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the download went on after its launch was cancelled")
	}
}
