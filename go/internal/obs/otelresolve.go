package obs

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// OtelEnvironmentNames are the OTEL_* variables LocalBase reads (TS
// OTEL_ENVIRONMENT_NAMES); a service definition carries them.
var OtelEnvironmentNames = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
}

var samplers = []string{"always_on", "always_off", "traceidratio", "parentbased_always_on", "parentbased_always_off",
	"parentbased_traceidratio"}

// OtelConfig is the resolved exporter configuration (TS OtelConfiguration):
// OTEL_* variables override the saved settings.
type OtelConfig struct {
	Enabled         bool
	TracesEndpoint  string
	LogsEndpoint    string
	Headers         map[string]string
	TracesHeaders   map[string]string
	LogsHeaders     map[string]string
	SampleRatio     float64
	Sampler         string
	Source          string // environment or persistent
	DisplayEndpoint string
}

// SignalEndpoint appends /v1/<signal> to a base endpoint's path.
func SignalEndpoint(base, signal string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/" + signal
	u.RawPath = ""
	return u.String()
}

// ResolveOtel resolves the saved endpoint, headers and sample percentage
// against the OTEL_* environment (TS resolveOtelConfiguration). An invalid
// variable is an error.
func ResolveOtel(endpoint, headers string, samplePercent int, getenv func(string) string) (*OtelConfig, error) {
	env := map[string]string{}
	for _, name := range OtelEnvironmentNames {
		if v := getenv(name); v != "" {
			env[name] = v
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		if v, ok := env[name]; ok {
			if msg := ValidateEndpoint(v); msg != "" {
				return nil, fmt.Errorf("%s: %s", name, msg)
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS"} {
		if v, ok := env[name]; ok {
			if _, err := ParseHeaders(v); err != nil {
				return nil, fmt.Errorf("%s: %s", name, err.Error())
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"} {
		if v, ok := env[name]; ok && v != "http/protobuf" {
			return nil, fmt.Errorf(`%s: Invalid input: expected "http/protobuf"`, name)
		}
	}
	sampler, hasSampler := env["OTEL_TRACES_SAMPLER"]
	if hasSampler && !slices.Contains(samplers, sampler) {
		return nil, fmt.Errorf("OTEL_TRACES_SAMPLER: Invalid option")
	}
	ratio := float64(samplePercent) / 100
	if arg, ok := env["OTEL_TRACES_SAMPLER_ARG"]; ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(arg), 64)
		if strings.TrimSpace(arg) == "" {
			f, err = 0, nil // Number("  ") is 0
		}
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f < 0 || f > 1 {
			return nil, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG: must be a number from 0 to 1")
		}
		if sampler != "traceidratio" && sampler != "parentbased_traceidratio" {
			return nil, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG: is only valid with traceidratio or parentbased_traceidratio")
		}
		ratio = f
	} else if hasSampler {
		ratio = 1
	}
	if !hasSampler {
		sampler = "parentbased_traceidratio"
	}
	if endpoint != "" {
		if msg := ValidateEndpoint(endpoint); msg != "" {
			return nil, fmt.Errorf("otelEndpoint: %s", msg)
		}
	}
	base := env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if base == "" {
		base = endpoint
	}
	c := &OtelConfig{Sampler: sampler, SampleRatio: ratio, Source: "persistent",
		TracesEndpoint: env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"], LogsEndpoint: env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"]}
	if c.TracesEndpoint == "" && base != "" {
		c.TracesEndpoint = SignalEndpoint(base, "traces")
	}
	if c.LogsEndpoint == "" && base != "" {
		c.LogsEndpoint = SignalEndpoint(base, "logs")
	}
	headerText, ok := env["OTEL_EXPORTER_OTLP_HEADERS"]
	if !ok {
		headerText = headers
	}
	var err error
	if c.Headers, err = ParseHeaders(headerText); err != nil {
		return nil, err
	}
	c.TracesHeaders, _ = ParseHeaders(env["OTEL_EXPORTER_OTLP_TRACES_HEADERS"])
	c.LogsHeaders, _ = ParseHeaders(env["OTEL_EXPORTER_OTLP_LOGS_HEADERS"])
	c.Enabled = c.TracesEndpoint != "" || c.LogsEndpoint != ""
	if len(env) > 0 {
		c.Source = "environment"
	}
	display := endpoint
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		if v, ok := env[name]; ok {
			display = v
			break
		}
	}
	c.DisplayEndpoint = SanitizedEndpoint(display)
	return c, nil
}
