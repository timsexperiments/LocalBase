package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The fingerprint is TS's: sha256 of JSON.stringify of the six static
// values (computed with Bun.CryptoHasher).
func TestStaticFingerprintMatchesTypeScript(t *testing.T) {
	c := &Config{GatewayHost: "127.0.0.1", GatewayPort: 2273,
		SystemReserve: MemoryReserve{Percent: 12.5, MinimumGB: 8}, AcceleratorReserve: MemoryReserve{Percent: 10, MinimumGB: 0.25}}
	if got, want := StaticFingerprint(c), "b956d4b9fdadc6e07f4f5fec43eff07cd4ca4ea5a6e8d9acc07a2ad989976f64"; got != want {
		t.Fatalf("fingerprint %s, want %s", got, want)
	}
}

func TestPendingRestartLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pending := func() bool {
		p, err := st.RestartPending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := &Config{Root: "/r", GatewayHost: "127.0.0.1", GatewayPort: 2273, Host: "127.0.0.1", Port: 18000, CtxSize: 4096,
		CtxMode: "auto", SttHost: "127.0.0.1", SttPort: 18080, Parallel: "auto",
		Selected: map[string][]string{"llm": {"m"}}, Active: map[string]string{"llm": "m"},
		SystemReserve: MemoryReserve{15, 8}, AcceleratorReserve: MemoryReserve{10, 2}}
	// The first save has nothing to compare with: a restart is pending.
	if err := st.SaveConfigMarking(ctx, cfg); err != nil || !pending() {
		t.Fatalf("first save: %v pending=%v", err, pending())
	}
	// A gateway started with other settings does not clear it.
	other := *cfg
	other.GatewayPort = 2274
	st.AcknowledgeStatic(ctx, &other)
	if !pending() {
		t.Fatal("acknowledged by a gateway with other settings")
	}
	if err := st.AcknowledgeStatic(ctx, cfg); err != nil || pending() {
		t.Fatalf("acknowledge: %v pending=%v", err, pending())
	}
	// Hot changes leave it clear; a static change sets it again.
	hot := *cfg
	hot.CtxSize = 8192
	st.SaveConfigMarking(ctx, &hot)
	if pending() {
		t.Fatal("a hot change marked a restart")
	}
	st.SaveConfigMarking(ctx, &other)
	if !pending() {
		t.Fatal("a listener change did not mark a restart")
	}
	// A gateway that started with the old settings cannot clear it, and a
	// re-mark after a failed restart is skipped once settings moved on.
	st.AcknowledgeStatic(ctx, cfg)
	if !pending() {
		t.Fatal("stale acknowledgement cleared the record")
	}
	st.AcknowledgeStatic(ctx, &other)
	st.MarkRestartPendingUnlessChanged(ctx, cfg)
	if pending() {
		t.Fatal("re-marked settings that are no longer saved")
	}
}
