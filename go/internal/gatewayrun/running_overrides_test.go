package gatewayrun

import (
	"io"
	"log/slog"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// These are what serve applies when it starts, so they are what a published configuration needs back.
func TestRunningOverridesAreTheEnvironmentAndTheFlag(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("LOCALBASE_HOST", "from-env.example")
	t.Setenv("LOCALBASE_PORT", "25401")
	c := &store.Config{Host: "saved.example", Port: 1}
	runningOverrides("", discard)(c)
	if c.Host != "from-env.example" || c.Port != 25401 {
		t.Errorf("from the environment: %q %d", c.Host, c.Port)
	}
	c = &store.Config{Host: "saved.example", Port: 1}
	runningOverrides("from-flag.example", discard)(c)
	if c.Host != "from-flag.example" || c.Port != 25401 {
		t.Errorf("the flag wins for the host, as at startup: %q %d", c.Host, c.Port)
	}
}
