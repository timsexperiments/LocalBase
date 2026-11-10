package gateway

import (
	"context"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// A management change through the gateway keeps the running overrides: model management is given
// the gateway's Overrides, applies them to what it publishes to the runtimes, and doesn't save
// them. admin/models' own test checks the same through its harness; this one checks the wiring.
func TestAManagementChangeThroughTheGatewayKeepsTheOverrides(t *testing.T) {
	const override = "override-token" // a field the fake LLM doesn't use
	ts, llm, _ := newConfiguredGateway(t, host{}, func(c *Config) {
		c.Overrides = func(sc *store.Config) { sc.HFToken = override }
	})
	got := post(t, ts, "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"activate"}`)
	if got.status != 200 {
		t.Fatalf("activate: %d %s", got.status, got.body)
	}
	if running := llm.Config().HFToken; running != override {
		t.Errorf("the running configuration lost the override: %q", running)
	}
	saved, err := runServer(ts).cfg.Store.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved.HFToken == override {
		t.Error("the override was saved")
	}
}
