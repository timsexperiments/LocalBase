package jsjson

import (
	"encoding/json"
	"os"
	"testing"
)

// TestGolden matches JSON.stringify(JSON.parse(input)) recorded from Bun.
func TestGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Input, Output string }
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		v, err := Parse([]byte(c.Input))
		if err != nil {
			t.Errorf("%s: %v", c.Input, err)
			continue
		}
		if got := string(Stringify(v)); got != c.Output {
			t.Errorf("%s\n got  %s\n want %s", c.Input, got, c.Output)
		}
	}
}
