package store

import (
	"context"
	"database/sql"
	"errors"
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

// Settings written with the configuration go in order, in the same transaction: one that can't be
// written leaves the configuration, the restart mark and the settings before it as they were.
func TestSaveConfigMarkingWritesSettingsInTheSameTransaction(t *testing.T) {
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
	if err := st.SaveConfigMarking(ctx, cfg, SettingValue{"a", "1"}, SettingValue{"b", "2"}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"a": "1", "b": "2"} {
		if v, _, _ := st.Setting(ctx, k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER refuse_c BEFORE INSERT ON go_settings WHEN NEW.key = 'c'
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	pending := func() string {
		t.Helper()
		var p sql.NullString
		if err := st.DB.QueryRowContext(ctx, `SELECT pending_static_config FROM config_activation WHERE id = 'default'`).Scan(&p); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return p.String
	}
	markBefore := pending()
	// The mark is real: the same listener change, saved without a refused setting, moves it.
	probe := *cfg
	probe.GatewayPort = 2275
	if err := st.SaveConfigMarking(ctx, &probe); err != nil {
		t.Fatal(err)
	}
	if pending() == markBefore {
		t.Fatal("a listener change did not mark a restart, so the check below says nothing")
	}
	if err := st.SaveConfigMarking(ctx, cfg); err != nil { // back to where it was
		t.Fatal(err)
	}
	markBefore = pending()
	changed := *cfg
	changed.GatewayPort = 2274 // a listener change marks a restart
	if err := st.SaveConfigMarking(ctx, &changed, SettingValue{"a", "9"}, SettingValue{"c", "3"}); err == nil {
		t.Fatal("setting c is refused, so the save should fail")
	}
	got, err := st.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.GatewayPort != 2273 {
		t.Errorf("the failed save left gateway port %d", got.GatewayPort)
	}
	if v, _, _ := st.Setting(ctx, "a"); v != "1" {
		t.Errorf("a = %q after the failed save, want 1", v)
	}
	if p := pending(); p != markBefore {
		t.Errorf("the failed save marked a restart: %q, was %q", p, markBefore)
	}
}
