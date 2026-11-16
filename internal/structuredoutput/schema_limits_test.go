package structuredoutput

import (
	"encoding/json"
	"testing"
)

// Twin of gateway's schemaWith (internal/gateway/schema_limits_test.go), which its HTTP tests use.
func schemaWith(property, defs string) string {
	return `{"type":"object","properties":{"p":` + property + `},"required":["p"],"additionalProperties":false,"$defs":{` + defs + `}}`
}

func decoded(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidationLimits(t *testing.T) {
	list := decoded(t, schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"type":"array","items":{"$ref":"#/$defs/a"}}`))
	nested := func(levels int) any {
		var v any = []any{}
		for range levels {
			v = []any{v}
		}
		return map[string]any{"p": v}
	}
	// Two steps a level: the reference, and the array it names.
	if !validateSchema(list, list, nested(maxValidationDepth/2-2)) {
		t.Error("an answer inside the depth limit failed")
	}
	if validateSchema(list, list, nested(maxValidationDepth/2+2)) {
		t.Error("an answer past the depth limit passed")
	}
	// Every step down counts, whichever way it goes.
	objects := decoded(t, schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"anyOf":[{"type":"integer"},{"type":"object","properties":{"x":{"$ref":"#/$defs/a"}}}]}`))
	var deep any = float64(1)
	for range maxValidationDepth / 3 {
		deep = map[string]any{"x": deep}
	}
	if validateSchema(objects, objects, map[string]any{"p": deep}) {
		t.Error("an object past the depth limit passed")
	}
	alternatives := decoded(t, schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"anyOf":[{"type":"array","items":{"$ref":"#/$defs/a"}}]}`))
	if validateSchema(alternatives, alternatives, nested(maxValidationDepth*2/5)) {
		t.Error("alternatives past the depth limit passed")
	}
	// Something large and ordinary fits in the budget.
	wide := decoded(t, schemaWith(`{"type":"array","items":{"type":"integer"}}`, ""))
	values := make([]any, 200_000)
	for i := range values {
		values[i] = float64(i)
	}
	if !validateSchema(wide, wide, map[string]any{"p": values}) {
		t.Error("a large answer failed")
	}
	if validateSchema(wide, wide, map[string]any{"p": make([]any, maxValidationSteps+1)}) {
		t.Error("an answer past the budget passed")
	}
	// The check holds by itself, even for a schema the gateway would have refused.
	looping := decoded(t, schemaWith(`{"$ref":"#/$defs/a"}`, `"a":{"$ref":"#/$defs/a"}`))
	if validateSchema(looping, looping, map[string]any{"p": float64(1)}) {
		t.Error("a schema that loops passed")
	}
}
