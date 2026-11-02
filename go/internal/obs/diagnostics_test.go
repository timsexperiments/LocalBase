package obs

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// TestEventsGolden replays tools/golden/logs.ts: the reader keeps exactly
// the lines TS's logEventSchema accepts, and a diagnostics bundle holds the
// same redacted event, byte for byte.
func TestEventsGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Line        string
		Accepted    bool
		Diagnostics string
	}
	if err := json.Unmarshal(b, &cases); err != nil || len(cases) < 40 {
		t.Fatalf("golden: %v (%d cases)", err, len(cases))
	}
	for _, c := range cases {
		e, ok := parseEvent([]byte(c.Line))
		if ok != c.Accepted {
			t.Errorf("accepted %v, want %v: %.200s", ok, c.Accepted, c.Line)
			continue
		}
		if !ok {
			continue
		}
		d, err := DiagnosticsEvent(e.Raw)
		if err != nil {
			t.Errorf("%v: %.200s", err, c.Line)
			continue
		}
		if got := string(jsjson.Stringify(d)); got != c.Diagnostics {
			t.Errorf("diagnostics:\n got  %.400s\n want %.400s", got, c.Diagnostics)
		}
	}
}

// Counts are metrics even when their names look like content; only a
// non-negative number gets through, so text under those names stays redacted.
func TestMetricAttributes(t *testing.T) {
	for _, key := range []string{"input_chars", "output_bytes", "prompt_tokens"} {
		if _, v, ok := attribute(key, 256); !ok || v != 256 {
			t.Errorf("%s = 256: %v %v", key, v, ok)
		}
		if _, v, _ := attribute(key, "the secret text"); v != redacted {
			t.Errorf("%s as text: %v", key, v)
		}
		if _, v, _ := attribute(key, -1); v != redacted {
			t.Errorf("%s = -1: %v", key, v)
		}
	}
	for _, key := range []string{"input", "input_text", "output", "output_text", "prompt"} {
		if _, v, _ := attribute(key, 256); v != redacted {
			t.Errorf("%s: %v, want redacted", key, v)
		}
	}
}
