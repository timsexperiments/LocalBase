package obs

import (
	"errors"
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
	u.Host = whatwgHost(u)
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
	// The TS otelEnvironmentSchema, issue for issue in schema order; a value
	// that is not a URL at all throws "Invalid URL" outright, as there.
	var issues []string
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		if v, ok := env[name]; ok {
			list, err := endpointIssues(v)
			if err != nil {
				return nil, err
			}
			for _, msg := range list {
				issues = append(issues, name+": "+msg)
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS"} {
		if v, ok := env[name]; ok {
			if len(v) > 8192 {
				issues = append(issues, name+": Too big: expected string to have <=8192 characters")
			} else if _, err := ParseHeaders(v); err != nil {
				issues = append(issues, name+": "+err.Error())
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"} {
		if v, ok := env[name]; ok && v != "http/protobuf" {
			issues = append(issues, name+`: Invalid input: expected "http/protobuf"`)
		}
	}
	sampler, hasSampler := env["OTEL_TRACES_SAMPLER"]
	if hasSampler && !slices.Contains(samplers, sampler) {
		issues = append(issues, `OTEL_TRACES_SAMPLER: Invalid option: expected one of "`+strings.Join(samplers, `"|"`)+`"`)
	}
	ratio := float64(samplePercent) / 100
	arg, hasArg := env["OTEL_TRACES_SAMPLER_ARG"]
	if hasArg {
		f := jsNumber(arg)
		switch {
		case math.IsNaN(f):
			issues = append(issues, "OTEL_TRACES_SAMPLER_ARG: Invalid input: expected number, received NaN")
		case math.IsInf(f, 0):
			issues = append(issues, "OTEL_TRACES_SAMPLER_ARG: Invalid input: expected number, received number")
		case f < 0:
			issues = append(issues, "OTEL_TRACES_SAMPLER_ARG: Too small: expected number to be >=0")
		case f > 1:
			issues = append(issues, "OTEL_TRACES_SAMPLER_ARG: Too big: expected number to be <=1")
		}
		ratio = f
	} else if hasSampler {
		ratio = 1
	}
	if len(issues) == 0 && hasArg && sampler != "traceidratio" && sampler != "parentbased_traceidratio" {
		issues = append(issues, "OTEL_TRACES_SAMPLER_ARG: is only valid with traceidratio or parentbased_traceidratio")
	}
	if len(issues) > 0 {
		return nil, errors.New(strings.Join(issues, "; "))
	}
	if !hasSampler {
		sampler = "parentbased_traceidratio"
	}
	if endpoint != "" {
		if msg := ValidateEndpoint(endpoint); msg != "" {
			return nil, errors.New(msg)
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

// endpointIssues validates an OTLP endpoint as otelEndpointSchema does; an
// unparseable value is an error, as TS's new URL() throws.
func endpointIssues(v string) ([]string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Opaque == "") {
		return nil, errors.New("Invalid URL")
	}
	var issues []string
	if len(v) > 2048 {
		issues = append(issues, "Too big: expected string to have <=2048 characters")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		issues = append(issues, "must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(v, "?") || strings.Contains(v, "#") {
		issues = append(issues, "must not contain credentials, query parameters, or a fragment; use OTLP headers for authentication")
	}
	return issues, nil
}

// jsNumber is JavaScript's Number(string) for the decimal forms an
// environment variable holds; anything else is NaN.
func jsNumber(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if strings.ContainsAny(t, "_xXoObBpPnN") && !strings.HasPrefix(strings.ToLower(t), "0x") {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if n, err := strconv.ParseInt(t, 0, 64); err == nil && strings.HasPrefix(strings.ToLower(t), "0x") {
			return float64(n)
		}
		return math.NaN()
	}
	return f
}
