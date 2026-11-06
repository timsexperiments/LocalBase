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

func TestIndent(t *testing.T) {
	v, _ := Parse([]byte(`{"a":[],"b":{},"c":[1,{"d":"x","e":[]}],"2":null,"f":1e21}`))
	want := "{\n  \"2\": null,\n  \"a\": [],\n  \"b\": {},\n  \"c\": [\n    1,\n    {\n      \"d\": \"x\",\n      \"e\": []\n    }\n  ],\n  \"f\": 1e+21\n}"
	if got := string(Indent(v)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
