package obs

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestResolveOtelGolden replays tools/golden/otelconfig.ts: saved settings
// under OTEL_* environments resolve to the TS configuration, or fail with
// the message the TS CLI prints.
func TestResolveOtelGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/otelconfig.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Config struct {
			OtelEndpoint    string `json:"otelEndpoint"`
			OtelHeaders     string `json:"otelHeaders"`
			OtelSampleRatio int    `json:"otelSampleRatio"`
		} `json:"config"`
		Env    map[string]string `json:"env"`
		Result *struct {
			Enabled         bool              `json:"enabled"`
			TracesEndpoint  *string           `json:"tracesEndpoint"`
			LogsEndpoint    *string           `json:"logsEndpoint"`
			Headers         map[string]string `json:"headers"`
			TracesHeaders   map[string]string `json:"tracesHeaders"`
			LogsHeaders     map[string]string `json:"logsHeaders"`
			SampleRatio     float64           `json:"sampleRatio"`
			Sampler         string            `json:"sampler"`
			Source          string            `json:"source"`
			DisplayEndpoint string            `json:"displayEndpoint"`
		} `json:"result"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(b, &cases); err != nil || len(cases) < 60 {
		t.Fatalf("golden: %v (%d)", err, len(cases))
	}
	for _, c := range cases {
		label := c.Config.OtelEndpoint + " " + func() string { b, _ := json.Marshal(c.Env); return string(b) }()
		got, err := ResolveOtel(c.Config.OtelEndpoint, c.Config.OtelHeaders, c.Config.OtelSampleRatio, func(k string) string { return c.Env[k] })
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("%s: error %v, want %q", label, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		r := c.Result
		str := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		want := OtelConfig{Enabled: r.Enabled, TracesEndpoint: str(r.TracesEndpoint), LogsEndpoint: str(r.LogsEndpoint),
			Headers: r.Headers, TracesHeaders: r.TracesHeaders, LogsHeaders: r.LogsHeaders, SampleRatio: r.SampleRatio,
			Sampler: r.Sampler, Source: r.Source, DisplayEndpoint: r.DisplayEndpoint}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", label, *got, want)
		}
	}
}
