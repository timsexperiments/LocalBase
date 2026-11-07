package library

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// The one built-in model with a variant other than "default", chosen by hand so its files are missing.
const otherModel, otherBackend, otherVariant = "kokoro-82m-v1.0-q8", "crispasr-cpu", "crispasr-f16"

// missingVariant is a library on a device that allows downloads, with that variant selected and its files
// missing. The model source counts what it is asked for and has nothing to give.
func missingVariant(t *testing.T) (*Library, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HF_ENDPOINT", srv.URL)
	l := newTestLibrary(t)
	l.InstallMissing = true
	if _, err := l.SetBackend(context.Background(), otherModel, otherBackend, otherVariant); err != nil {
		t.Fatal(err)
	}
	return l, &asked
}

// Only a launch fetches what a model is missing. A question about the model gets its target and "not
// installed", and downloads nothing, even on a device that allows downloads.
func TestAQuestionDownloadsNothing(t *testing.T) {
	l, asked := missingVariant(t)
	gate := l.GateFor("speech")
	target, err := gate(context.Background(), otherModel, false)
	var missing *runtime.NotInstalledError
	if !errors.As(err, &missing) || target == nil || target.Variant == nil || target.Variant.ID != otherVariant {
		t.Fatalf("a question got a target %v and %v; want the %s target and not installed", target != nil, err, otherVariant)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("a question asked for the model's files %d times", n)
	}
	// The same call from a launch goes to the source.
	if _, err := gate(context.Background(), otherModel, true); err == nil {
		t.Fatal("a launch got the model from a source that has nothing")
	}
	if asked.Load() == 0 {
		t.Fatal("a launch didn't fetch what its model is missing")
	}
}
