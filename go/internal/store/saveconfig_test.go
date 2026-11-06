package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The configuration and the default-model setting are saved together or not at all: the second
// statement failing must not leave the first one saved.
func TestSaveConfigIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &Config{Root: "/r", GatewayHost: "127.0.0.1", GatewayPort: 2273, Host: "127.0.0.1", Port: 18000, CtxSize: 8192,
		CtxMode: "auto", SttHost: "127.0.0.1", SttPort: 18080, Parallel: "auto",
		Selected: map[string][]string{"llm": {"m"}}, Active: map[string]string{"llm": "m"},
		SystemReserve: MemoryReserve{15, 8}, AcceleratorReserve: MemoryReserve{10, 2}}
	if err := st.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `DROP TABLE go_settings`); err != nil {
		t.Fatal(err)
	}
	changed := *cfg
	changed.Port = 18001
	if err := st.SaveConfig(ctx, &changed); err == nil {
		t.Fatal("the setting's table is gone, so the save should fail")
	}
	got, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 18000 {
		t.Errorf("the failed save left port %d", got.Port)
	}
}
