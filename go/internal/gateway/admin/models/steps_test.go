package models

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The finding: a management change published the saved configuration, so an override from the
// environment or a serve flag disappeared from the running gateway after an unrelated change.
func TestAManagementChangeKeepsTheRunningOverrides(t *testing.T) {
	m := newModelsHarness(t, func(c *store.Config) { c.Host = "override.example" })
	llm := m.llm
	over := llm.Config().Clone()
	m.overrides(over)
	llm.SetConfig(over)
	savedHost := func() string {
		t.Helper()
		saved, err := m.store.LoadConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return saved.Host
	}
	before := savedHost()
	if err := m.h.updateConfig(func(c *store.Config) error {
		c.Parallel = "2"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := llm.Config()
	if got.Host != "override.example" {
		t.Errorf("the override went: running host %q", got.Host)
	}
	if got.Parallel != "2" {
		t.Errorf("the change didn't reach the running configuration: %q", got.Parallel)
	}
	if savedHost() != before {
		t.Errorf("the override was saved: %q", savedHost())
	}
}

// What another process saved still arrives with a management change, as it did.
func TestAManagementChangePublishesWhatWasSavedElsewhere(t *testing.T) {
	m := newModelsHarness(t, func(c *store.Config) { c.Host = "override.example" })
	llm := m.llm
	saved, err := m.store.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved.HFToken = "saved-elsewhere"
	if err := m.store.SaveConfig(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if err := m.h.updateConfig(func(c *store.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := llm.Config(); got.HFToken != "saved-elsewhere" || got.Host != "override.example" {
		t.Errorf("host %q, token set %v", got.Host, got.HFToken != "")
	}
}

// With no overrides it is the saved configuration.
func TestAManagementChangeWithNoOverridesPublishesTheSavedConfiguration(t *testing.T) {
	m := newModelsHarness(t)
	llm := m.llm
	if err := m.h.updateConfig(func(c *store.Config) error {
		c.Parallel = "3"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if llm.Config().Parallel != "3" {
		t.Errorf("running %q", llm.Config().Parallel)
	}
}

// A failed install says it can go on only when there is a part to go on from.
func TestAFailedInstallMentionsAPartOnlyWhenOneWasKept(t *testing.T) {
	m := newModelsHarness(t)
	var spec *catalog.Spec
	for _, c := range m.cat.All() {
		if len(c.Artifacts) > 0 {
			spec = c
			break
		}
	}
	const failed = "Installation failed. Check storage, network access, and catalog verification."
	if got := m.h.failedInstallDetail(spec); got != failed {
		t.Errorf("with no part: %q", got)
	}
	dir := m.root.ModelsDir(spec.Kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, spec.Artifacts[0].Filename) + ".partial"
	if err := os.WriteFile(part, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.h.failedInstallDetail(spec); got != failed {
		t.Errorf("with an empty part, nothing to go on from: %q", got)
	}
	if err := os.WriteFile(part, []byte("some"), 0o600); err != nil {
		t.Fatal(err)
	}
	const want = failed + " The downloaded part is kept, so installing again continues from it (or starts over if the file has changed)."
	if got := m.h.failedInstallDetail(spec); got != want {
		t.Errorf("with a part: %q", got)
	}
}
