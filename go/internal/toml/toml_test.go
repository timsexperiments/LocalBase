package toml

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// normalize turns parsed values into what encoding/json would produce, so
// they compare with Bun.TOML.parse output (numbers become float64).
func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

func TestMatchesBun(t *testing.T) {
	b, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Doc    string
		Parsed any
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		got, err := Parse(c.Doc)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if g := normalize(got); !reflect.DeepEqual(g, c.Parsed) {
			gb, _ := json.Marshal(g)
			wb, _ := json.Marshal(c.Parsed)
			t.Errorf("case %d:\n go  %s\n bun %s", i, gb, wb)
		}
	}
}

func TestErrors(t *testing.T) {
	for _, doc := range []string{"a = ", "a = 1\na = 2", "[t]\n[t]", "a = \"unterminated", "a = [1, 2", "a = 1 b = 2", "a = \"\\q\""} {
		if _, err := Parse(doc); err == nil {
			t.Errorf("%q parsed", doc)
		}
	}
}
