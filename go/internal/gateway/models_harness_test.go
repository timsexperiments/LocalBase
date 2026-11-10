package gateway

import (
	"io"
	"log/slog"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The tests that call into model management's own steps run against a Handler alone, on a scratch
// root with the built-in catalog and a real LLM runtime that is never started: what they test is
// what the Handler writes and publishes.

// modelsHarness is a Handler on a scratch root.
type modelsHarness struct {
	h     *Handler
	root  *root.Root
	store *store.Store
	cat   *catalog.Catalog
	llm   *runtime.LLM
	// overrides is what the running gateway applies on top of the saved configuration, read when
	// the Handler asks, as the gateway's Overrides is.
	overrides func(*store.Config)
}

func newModelsHarness(t *testing.T) *modelsHarness {
	t.Helper()
	r, err := root.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	// A configured root, as the gateway's tests have: the LLM is never started, so its port is unused.
	cfg := &store.Config{Host: "127.0.0.1", Port: 25499, CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active: map[string]string{"llm": gatewaytest.ChatModel}, Selected: map[string][]string{"llm": {gatewaytest.ChatModel}}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	if err := st.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), Store: st, Catalog: cat}, cfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &modelsHarness{root: r, store: st, cat: cat, llm: llm}
	m.h = NewHandler(Options{
		Store:     st,
		Root:      r,
		Catalog:   cat,
		Runtimes:  func() Runtimes { return Runtimes{LLM: llm} },
		Log:       func() *slog.Logger { return log },
		NoAuth:    func() bool { return false },
		Library:   func() *library.Library { return nil },
		Overrides: func() func(*store.Config) { return m.overrides },
	})
	return m
}
