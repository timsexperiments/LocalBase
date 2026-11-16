package otlp

import (
	"context"
	"log/slog"
	"os"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

// Start starts OTLP export when OTEL_* or the saved settings (endpoint, headers, samplePercent)
// enable it (TS activateContextOtel): every event the gateway logs is
// exported with its span correlation, except the exporter's own
// diagnostics, which stay in the local log.
func Start(endpoint, headers string, samplePercent int, handler *obs.Handler, log *slog.Logger) (*Runtime, error) {
	oc, err := obs.ResolveOtel(endpoint, headers, samplePercent, os.Getenv)
	if err != nil {
		return nil, err
	}
	tel := New(oc, version.Version, func(severity, message string, attrs map[string]any) {
		event, level := "observability.export-failed", slog.LevelWarn
		switch severity {
		case "info":
			event, level = "observability.export-recovered", slog.LevelInfo
		case "error":
			level = slog.LevelError
		}
		args := []any{"event", event, "category", "logging", "component", "otel", obs.LocalOnly, true}
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, k, attrs[k])
		}
		log.Log(context.Background(), level, message, args...)
	})
	if tel.Enabled() {
		hooks := handler.Hooks()
		hooks.Correlate = Correlation
		hooks.Export = func(line []byte) {
			if v, err := jsjson.Parse(line); err == nil {
				if o, ok := v.(*jsjson.Object); ok {
					tel.Emit(o)
				}
			}
		}
	}
	return tel, nil
}
