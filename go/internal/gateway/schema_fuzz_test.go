package gateway

import (
	"encoding/json"
	"testing"
)

// FuzzStructuredOutput feeds arbitrary schemas and answers to what a chat key can reach. Plain
// `go test` runs the seeds; tools/fuzz.sh fuzzes on demand. It must never panic or overflow the
// stack, whatever it accepts.
func FuzzStructuredOutput(f *testing.F) {
	a := `{"$ref":"#/$defs/a"}`
	f.Add(`{"type":"object","properties":{"p":{"type":"integer"}},"required":["p"],"additionalProperties":false}`, `{"p":1}`)
	f.Add(schemaWith(a, `"a":`+a), `{"p":1}`)
	f.Add(schemaWith(a, `"a":{"anyOf":[`+a+`,{"type":"integer"}]}`), `{"p":1}`)
	f.Add(schemaWith(a, `"a":{"type":"array","items":`+a+`}`), `{"p":[[],[[]]]}`)
	f.Add(fanOut(5), `{"p":"x"}`)
	f.Add(fanOut(40), `{"p":1}`)
	f.Add(`{"type":"object","$defs":{"$ref":"x"}}`, `{}`)
	f.Add(`{"type":"object","properties":{"$ref":{"type":"string"}}}`, `{"$ref":"x"}`)
	f.Add(`{`, ``)
	f.Fuzz(func(t *testing.T, schema, answer string) {
		var s any
		if json.Unmarshal([]byte(schema), &s) != nil {
			return
		}
		accepted, rejection := Prepare(map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "schema": s}})
		if rejection != nil || accepted == nil {
			return
		}
		resp := map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": answer}}}}
		switch outcome := Outcome(resp, accepted); outcome {
		case "passed", "failed":
		default:
			t.Errorf("outcome %q", outcome)
		}
	})
}
