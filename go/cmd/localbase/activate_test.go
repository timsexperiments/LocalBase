package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type heldConfig struct {
	mu  sync.Mutex
	cfg *store.Config
}

func (h *heldConfig) Config() *store.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *heldConfig) SetConfig(c *store.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = c
}

// activationRoot is a root with a saved configuration, and a gateway's running copy of it.
func activationRoot(t *testing.T) (*root.Root, *store.Store, *heldConfig) {
	t.Helper()
	t.Setenv("LOCALBASE_ROOT", "")
	dir := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", dir, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	r, err := root.Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r, st, &heldConfig{cfg: cfg.Clone()}
}

// The finding: each activation saved what this process remembered, so activations at once lost each
// other's kinds.
func TestActivationsAtOnceAllLand(t *testing.T) {
	r, st, running := activationRoot(t)
	want := map[string]string{"llm": "model-a", "stt": "model-b", "tts": "model-c", "image": "model-d", "video": "model-e"}
	var wg sync.WaitGroup
	errs := make(chan error, len(want))
	for kind, id := range want {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- conf.RecordActive(r.Dir, st, running, kind, id)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	saved, err := st.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for kind, id := range want {
		if saved.Active[kind] != id {
			t.Errorf("saved %s: %q, want %q", kind, saved.Active[kind], id)
		}
		if got := running.Config().Active[kind]; got != id {
			t.Errorf("running %s: %q, want %q", kind, got, id)
		}
	}
}

// And a freshly saved token was overwritten by the one this process remembered.
func TestAnActivationKeepsAFreshlySavedToken(t *testing.T) {
	r, st, running := activationRoot(t)
	ctx := context.Background()
	saved, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved.HFToken = "saved-after-the-gateway-started"
	if err := st.SaveConfig(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if err := conf.RecordActive(r.Dir, st, running, "stt", "model-b"); err != nil {
		t.Fatal(err)
	}
	after, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.HFToken != saved.HFToken || after.Active["stt"] != "model-b" {
		t.Error("the saved token or the activation was lost")
	}
	if running.Config().HFToken != saved.HFToken {
		t.Error("the running configuration has an old token")
	}
}

// What only this process has (the environment, serve's flags) isn't saved, and isn't lost from it.
func TestAnActivationDoesNotSaveTheRunningOverrides(t *testing.T) {
	r, st, running := activationRoot(t)
	ctx := context.Background()
	savedHost := running.Config().Host
	over := running.Config().Clone()
	over.Host = "override.example"
	running.SetConfig(over)
	if err := conf.RecordActive(r.Dir, st, running, "stt", "model-b"); err != nil {
		t.Fatal(err)
	}
	saved, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Host != savedHost {
		t.Errorf("saved host %q, want %q", saved.Host, savedHost)
	}
	if running.Config().Host != "override.example" {
		t.Errorf("running host %q", running.Config().Host)
	}
}

// Already active is nothing to do, not a write.
func TestActivatingTheActiveModelWritesNothing(t *testing.T) {
	r, st, running := activationRoot(t)
	ctx := context.Background()
	if err := conf.RecordActive(r.Dir, st, running, "stt", "model-b"); err != nil {
		t.Fatal(err)
	}
	saved, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved.Active["stt"] = "changed-elsewhere"
	if err := st.SaveConfig(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if err := conf.RecordActive(r.Dir, st, running, "stt", "model-b"); err != nil {
		t.Fatal(err)
	}
	after, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Active["stt"] != "changed-elsewhere" {
		t.Errorf("wrote %q", after.Active["stt"])
	}
}

// serve's on-demand install runs long after it read its configuration.
func TestAnOnDemandInstallUsesTheTokenSavedSinceStartup(t *testing.T) {
	_, st, running := activationRoot(t)
	ctx := context.Background()
	e := &env{Store: st, Config: running.Config()}
	saved, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved.HFToken = "saved-after-the-gateway-started"
	if err := st.SaveConfig(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if got := install.CurrentHFToken(ctx, e.Store, e.Config); got != saved.HFToken {
		t.Error("the install would use the token from startup")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	e.Config = &store.Config{HFToken: "from-startup"}
	if got := install.CurrentHFToken(ctx, e.Store, e.Config); got != "from-startup" {
		t.Error("with the store unreadable, it should fall back to what it started with")
	}
}
