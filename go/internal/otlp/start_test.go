package otlp

import (
	"log/slog"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

func startHandler(t *testing.T) *obs.Handler {
	t.Helper()
	sink, err := obs.OpenSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	return obs.NewHandler(sink, slog.LevelInfo)
}

// With nothing configured, nothing is exported and the log's hooks stay unset; with an endpoint,
// every event the gateway logs is exported, correlated; a bad OTEL_* variable is refused before
// anything starts.
func TestStart(t *testing.T) {
	for _, name := range obs.OtelEnvironmentNames { // every one Start reads
		t.Setenv(name, "")
	}
	t.Run("off", func(t *testing.T) {
		h := startHandler(t)
		tel, err := Start("", "", 100, h, slog.New(h))
		if err != nil || tel.Enabled() {
			t.Fatalf("%v, enabled %v", err, tel != nil && tel.Enabled())
		}
		defer tel.Shutdown()
		if h.Hooks().Correlate != nil || h.Hooks().Export != nil {
			t.Error("the hooks were set with nothing to export")
		}
	})
	t.Run("on", func(t *testing.T) {
		h := startHandler(t)
		tel, err := Start("http://127.0.0.1:1", "", 100, h, slog.New(h))
		if err != nil || !tel.Enabled() {
			t.Fatalf("%v, enabled %v", err, tel != nil && tel.Enabled())
		}
		defer tel.Shutdown()
		if h.Hooks().Correlate == nil || h.Hooks().Export == nil {
			t.Error("an enabled export didn't set the hooks")
		}
	})
	t.Run("a bad variable", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_SAMPLER", "nonsense")
		h := startHandler(t)
		if tel, err := Start("", "", 100, h, slog.New(h)); err == nil {
			tel.Shutdown()
			t.Error("a bad OTEL_TRACES_SAMPLER was accepted")
		}
	})
}
