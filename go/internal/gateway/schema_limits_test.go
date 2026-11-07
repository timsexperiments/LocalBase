package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A schema is a small request that can ask for a lot of work: here, from the gateway's own
// check of the answer, and from the backend that turns it into a grammar.

func schemaWith(property, defs string) string {
	return `{"type":"object","properties":{"p":` + property + `},"required":["p"],"additionalProperties":false,"$defs":{` + defs + `}}`
}

func schemaRequest(t *testing.T, schema, reply string) string {
	t.Helper()
	content, err := json.Marshal("fake:reply " + reply)
	if err != nil {
		t.Fatal(err)
	}
	return `{"model":"` + chatModel + `","messages":[{"role":"user","content":` + string(content) + `}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"n","schema":` + schema + `}}}`
}

// fanOut doubles the alternatives at each level, in a schema of a few hundred bytes.
func fanOut(levels int) string {
	var defs []string
	for i := 1; i <= levels; i++ {
		next := fmt.Sprintf(`{"$ref":"#/$defs/a%d"}`, i+1)
		defs = append(defs, fmt.Sprintf(`"a%d":{"anyOf":[%s,%s]}`, i, next, next))
	}
	defs = append(defs, fmt.Sprintf(`"a%d":{"type":"string"}`, levels+1))
	return schemaWith(`{"$ref":"#/$defs/a1"}`, strings.Join(defs, ","))
}

// References that come back to where they started, with nothing consumed on the way, can
// never finish. They are refused before a model is looked up or loaded.
func TestSchemaReferencesThatLoopAreRefused(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	a := `{"$ref":"#/$defs/a"}`
	cases := map[string]string{
		"to itself":            schemaWith(a, `"a":`+a),
		"through another":      schemaWith(a, `"a":{"$ref":"#/$defs/b"},"b":`+a),
		"through three":        schemaWith(a, `"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/c"},"c":`+a),
		"first alternative":    schemaWith(a, `"a":{"anyOf":[`+a+`,{"type":"integer"}]}`),
		"last alternative":     schemaWith(a, `"a":{"anyOf":[{"type":"integer"},`+a+`]}`),
		"nested alternatives":  schemaWith(a, `"a":{"anyOf":[{"type":"integer"},{"anyOf":[{"type":"string"},`+a+`]}]}`),
		"under items":          schemaWith(`{"type":"array","items":`+a+`}`, `"a":`+a),
		"that nothing reaches": schemaWith(`{"type":"integer"}`, `"a":`+a),
		"in definitions": `{"type":"object","properties":{"p":{"$ref":"#/definitions/a"}},"required":["p"],"additionalProperties":false,` +
			`"definitions":{"a":{"$ref":"#/definitions/a"}}}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			r := post(t, ts, "/v1/chat/completions", schemaRequest(t, schema, `{"p":1}`))
			if r.status != 400 || r.code() != "unsupported_json_schema" || !strings.Contains(r.body, "loop") {
				t.Errorf("%d %s: %s", r.status, r.code(), r.body)
			}
		})
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("a backend was started for %q", id)
	}
}

// A reference through a property or an item consumes part of the answer, so it ends when the
// answer does. Those keep working, and their answers are still checked.
func TestRecursiveSchemasThatConsumeInputStillWork(t *testing.T) {
	ts := newTestServer(t)
	tree := schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"type":"object","properties":{"next":{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null"}]}},"required":["next"],"additionalProperties":false}`)
	list := schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"type":"array","items":{"$ref":"#/$defs/a"}}`)
	cases := []struct {
		name, schema, reply string
		status              int
	}{
		{"a tree", tree, `{"p":{"next":{"next":null}}}`, 200},
		{"a wrong tree", tree, `{"p":{"next":{"next":1}}}`, 502},
		{"a list", list, `{"p":[[],[[]]]}`, 200},
		{"a wrong list", list, `{"p":[[],[1]]}`, 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := post(t, ts, "/v1/chat/completions", schemaRequest(t, c.schema, c.reply)); r.status != c.status {
				t.Errorf("%d: %s", r.status, r.body)
			}
		})
	}
}

// Alternatives that multiply are cheap to write and expensive to check.
func TestSchemaAlternativesAreBounded(t *testing.T) {
	ts := newTestServer(t)
	wide := func(n int) string {
		return schemaWith(`{"anyOf":[`+strings.TrimSuffix(strings.Repeat(`{"type":"string"},`, n), ",")+`]}`, "")
	}
	cases := []struct {
		name, schema string
		accepted     bool
	}{
		{"10 levels, 1024 ways", fanOut(10), true},
		{"11 levels, 2048 ways", fanOut(11), false},
		{"40 levels", fanOut(40), false},
		{"70 levels, more ways than a number holds", fanOut(70), false},
		{"1024 alternatives", wide(1024), true},
		{"1025 alternatives", wide(1025), false},
		{"under a property of a definition", schemaWith(`{"$ref":"#/$defs/o"}`, `"o":{"type":"object","properties":{"q":{"$ref":"#/$defs/a1"}},"required":["q"],"additionalProperties":false},`+
			strings.TrimSuffix(strings.TrimPrefix(fanOut(11)[strings.Index(fanOut(11), `"$defs":{`):], `"$defs":{`), "}}")), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			r := post(t, ts, "/v1/chat/completions", schemaRequest(t, c.schema, `{"p":"x"}`))
			if c.accepted && r.status != 200 {
				t.Errorf("%d: %s", r.status, r.body)
			}
			if !c.accepted && (r.status != 400 || r.code() != "unsupported_json_schema" || !strings.Contains(r.body, "alternatives")) {
				t.Errorf("%d %s: %s", r.status, r.code(), r.body)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

// Each level of this one offers the same object twice, so an answer that fails at the bottom
// would be checked twice for every level above it. The check stops, and says the answer failed.
func TestCheckingAnAnswerIsBounded(t *testing.T) {
	ts := newTestServer(t)
	object := `{"type":"object","properties":{"x":{"$ref":"#/$defs/a"}},"required":["x"],"additionalProperties":false}`
	schema := schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"anyOf":[`+object+`,`+object+`,{"type":"integer"}]}`)
	reply := `{"p":` + strings.Repeat(`{"x":`, 60) + `"not an integer"` + strings.Repeat(`}`, 60) + `}`
	done := make(chan result, 1)
	go func() { done <- post(t, ts, "/v1/chat/completions", schemaRequest(t, schema, reply)) }()
	select {
	case r := <-done:
		if r.status != 502 || r.code() != "structured_output_validation_failed" {
			t.Errorf("%d %s: %s", r.status, r.code(), r.body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the check did not stop")
	}
}
