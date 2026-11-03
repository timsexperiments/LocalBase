package gateway

import (
	"context"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The finding: a management change published the saved configuration, so an override from the
// environment or a serve flag disappeared from the running gateway after an unrelated change.
func TestAManagementChangeKeepsTheRunningOverrides(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.Overrides = func(c *store.Config) { c.Host = "override.example" }
	over := llm.Config().Clone()
	s.Overrides(over)
	llm.SetConfig(over)
	savedHost := func() string {
		t.Helper()
		saved, err := s.Store.LoadConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return saved.Host
	}
	before := savedHost()
	if err := s.updateConfig(func(c *store.Config) error {
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
	ts, llm, _ := newTestGateway(t, host{})
	s := runServer(ts)
	s.Overrides = func(c *store.Config) { c.Host = "override.example" }
	saved, err := s.Store.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved.HFToken = "saved-elsewhere"
	if err := s.Store.SaveConfig(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if err := s.updateConfig(func(c *store.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := llm.Config(); got.HFToken != "saved-elsewhere" || got.Host != "override.example" {
		t.Errorf("host %q, token set %v", got.Host, got.HFToken != "")
	}
}

// With no overrides it is the saved configuration.
func TestAManagementChangeWithNoOverridesPublishesTheSavedConfiguration(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	s := runServer(ts)
	if err := s.updateConfig(func(c *store.Config) error {
		c.Parallel = "3"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if llm.Config().Parallel != "3" {
		t.Errorf("running %q", llm.Config().Parallel)
	}
}
