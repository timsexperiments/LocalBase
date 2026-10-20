package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Structured output (response_format json_schema), ported from
// structured-output.ts without a JSON Schema library. A schema is accepted
// only inside the subset llama-server's grammar converter handles, so the
// validator below only needs that subset. ajv's strict-mode compile errors
// (unknown keywords, malformed keyword values, missing types, undefined
// required properties, unresolved references) are reproduced as
// invalid_json_schema; subset violations are unsupported_json_schema.

const (
	maxSchemaDepth           = 10
	maxSchemaProperties      = 5000
	maxSchemaObjectProps     = 1000
	maxSchemaBytes           = 256 * 1024
	maxSchemaReferences      = 1000
	maxNativeRepetition      = 10000
	draft7URI                = "http://json-schema.org/draft-07/schema#"
	codeInvalidJSONSchema    = "invalid_json_schema"
	codeUnsupportedJSONSchma = "unsupported_json_schema"
)

var (
	annotationKeywords = []string{"$comment", "$schema", "description", "title"}
	supportedKeywords  = []string{"$defs", "$ref", "additionalProperties", "anyOf", "const", "definitions", "enum",
		"exclusiveMaximum", "exclusiveMinimum", "format", "items", "maxItems", "maxLength", "maximum", "minItems",
		"minLength", "minimum", "properties", "required", "type"}
	supportedRefRe = regexp.MustCompile(`^#/(?:\$defs|definitions)/[A-Za-z0-9_-]+$`)
)

type schemaRejection struct{ code, message string }

func (e *schemaRejection) Error() string { return e.message }

// prepareStructuredOutput returns the root schema to validate completed
// responses against, nil when the request has no json_schema format, or a
// rejection.
func prepareStructuredOutput(responseFormat any) (map[string]any, *schemaRejection) {
	rf, _ := responseFormat.(map[string]any)
	if rf == nil || rf["type"] != "json_schema" {
		return nil, nil
	}
	js, _ := rf["json_schema"].(map[string]any)
	schema, _ := js["schema"].(map[string]any)
	if b, _ := json.Marshal(schema); len(b) > maxSchemaBytes {
		return nil, &schemaRejection{codeUnsupportedJSONSchma, "Structured output schema exceeds LocalBase's 256 KiB compile limit."}
	}
	if msg := precompileBudgetIssue(schema); msg != "" {
		return nil, &schemaRejection{codeUnsupportedJSONSchma, msg}
	}
	if s, present := schema["$schema"]; present && s != draft7URI {
		return nil, &schemaRejection{codeUnsupportedJSONSchma, "Structured output schemas must use JSON Schema draft 7."}
	}
	if err := strictCompile(schema, schema, nil); err != nil {
		return nil, &schemaRejection{codeInvalidJSONSchema, "Structured output schema is not valid JSON Schema draft 7."}
	}
	if msg := inspectNative(schema); msg != "" {
		return nil, &schemaRejection{codeUnsupportedJSONSchma, msg}
	}
	if schema["type"] != "object" {
		return nil, &schemaRejection{codeUnsupportedJSONSchma, "Structured output schema root must be an object type."}
	}
	return schema, nil
}

// childSchemas lists the subschemas the subset can nest, or nil, false when
// a container is malformed.
func childSchemas(s map[string]any) ([]map[string]any, bool) {
	var out []map[string]any
	for _, key := range []string{"properties", "$defs", "definitions"} {
		v, present := s[key]
		if !present {
			continue
		}
		m, ok := v.(map[string]any)
		if _, hasRef := m["$ref"]; !ok || hasRef { // b10419 mistakes entries named "$ref" for references
			return nil, false
		}
		for _, k := range sortedKeys(m) {
			child, ok := m[k].(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, child)
		}
	}
	if v, present := s["items"]; present {
		child, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, child)
	}
	if v, present := s["anyOf"]; present {
		list, ok := v.([]any)
		if !ok {
			return nil, false
		}
		for _, e := range list {
			child, ok := e.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, child)
		}
	}
	return out, true
}

func precompileBudgetIssue(root map[string]any) string {
	type item struct {
		s     map[string]any
		depth int
	}
	props, refs := 0, 0
	pending := []item{{root, 1}}
	for len(pending) > 0 {
		cur := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if cur.depth > maxSchemaDepth {
			return "Structured output schema exceeds OpenAI's nesting limit."
		}
		if p, ok := cur.s["properties"].(map[string]any); ok {
			if len(p) > maxSchemaObjectProps {
				return "Structured output schema object exceeds LocalBase's 1,000-property compilation limit."
			}
			props += len(p)
			if props > maxSchemaProperties {
				return "Structured output schema exceeds OpenAI's property limit."
			}
		}
		if ref, present := cur.s["$ref"]; present {
			refs++
			if refs > maxSchemaReferences {
				return "Structured output schema exceeds LocalBase's reference limit."
			}
			if r, ok := ref.(string); ok && !supportedRefRe.MatchString(r) {
				return "Structured output schema references must target direct local definitions."
			}
		}
		children, ok := childSchemas(cur.s)
		if !ok {
			continue
		}
		for _, c := range children {
			pending = append(pending, item{c, cur.depth + 1})
		}
	}
	return ""
}

// --- ajv strict-mode compile emulation -----------------------------------------

var (
	simpleTypes   = []string{"array", "boolean", "integer", "null", "number", "object", "string"}
	ajvKeywords   = []string{"$id", "$schema", "$ref", "$comment", "$defs", "definitions", "title", "description", "default", "examples", "readOnly", "writeOnly", "type", "enum", "const", "nullable", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum", "maxLength", "minLength", "pattern", "format", "items", "additionalItems", "maxItems", "minItems", "uniqueItems", "contains", "maxProperties", "minProperties", "required", "properties", "patternProperties", "additionalProperties", "dependencies", "propertyNames", "if", "then", "else", "allOf", "anyOf", "oneOf", "not", "contentMediaType", "contentEncoding"}
	keywordTypes  = map[string]string{"multipleOf": "number", "maximum": "number", "exclusiveMaximum": "number", "minimum": "number", "exclusiveMinimum": "number", "maxLength": "string", "minLength": "string", "pattern": "string", "format": "string", "items": "array", "additionalItems": "array", "maxItems": "array", "minItems": "array", "uniqueItems": "array", "contains": "array", "maxProperties": "object", "minProperties": "object", "required": "object", "properties": "object", "patternProperties": "object", "additionalProperties": "object", "dependencies": "object", "propertyNames": "object"}
	errStrict     = errors.New("strict schema error")
	nonNegIntKeys = []string{"maxLength", "minLength", "maxItems", "minItems", "maxProperties", "minProperties"}
	numberKeys    = []string{"maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum"}
	schemaKeys    = []string{"additionalProperties", "additionalItems", "contains", "propertyNames", "not", "if", "then", "else"}
	schemaMapKeys = []string{"properties", "patternProperties", "definitions", "$defs"}
	schemaArrKeys = []string{"allOf", "anyOf", "oneOf"}
)

func isSchema(v any) bool {
	switch v.(type) {
	case bool, map[string]any:
		return true
	}
	return false
}

func schemaTypesOf(s map[string]any) []string {
	switch t := s["type"].(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func applicable(types []string, kwType string) bool {
	return contains(types, kwType) || (kwType == "number" && contains(types, "integer"))
}

// strictCompile reports what ajv (strict, allowUnionTypes) refuses to
// compile. ctx are the data types known from the parent, as ajv threads
// them into same-data subschemas (anyOf, allOf, not, if/then/else).
func strictCompile(root, s map[string]any, ctx []string) error {
	for k, v := range s {
		if !contains(ajvKeywords, k) {
			return errStrict
		}
		switch {
		case k == "type":
			switch t := v.(type) {
			case string:
				if !contains(simpleTypes, t) {
					return errStrict
				}
			case []any:
				seen := map[string]bool{}
				for _, e := range t {
					str, ok := e.(string)
					if !ok || !contains(simpleTypes, str) || seen[str] {
						return errStrict
					}
					seen[str] = true
				}
			default:
				return errStrict
			}
		case k == "enum" || k == "examples":
			if _, ok := v.([]any); !ok {
				return errStrict
			}
		case k == "multipleOf":
			if f, ok := number(v); !ok || f <= 0 {
				return errStrict
			}
		case contains(numberKeys, k):
			if _, ok := number(v); !ok {
				return errStrict
			}
		case contains(nonNegIntKeys, k):
			if f, ok := number(v); !ok || f < 0 || f != math.Trunc(f) {
				return errStrict
			}
		case k == "required":
			list, ok := v.([]any)
			if !ok {
				return errStrict
			}
			seen := map[string]bool{}
			props, _ := s["properties"].(map[string]any)
			for _, e := range list {
				str, ok := e.(string)
				if !ok || seen[str] {
					return errStrict
				}
				seen[str] = true
				if _, defined := props[str]; !defined { // strictRequired
					return errStrict
				}
			}
		case k == "uniqueItems" || k == "readOnly" || k == "writeOnly" || k == "nullable":
			if _, ok := v.(bool); !ok {
				return errStrict
			}
		case k == "$ref":
			ref, ok := v.(string)
			if !ok || !resolvable(root, ref) {
				return errStrict
			}
		case k == "pattern":
			str, ok := v.(string)
			if !ok {
				return errStrict
			}
			if _, err := regexp.Compile(str); err != nil {
				return errStrict
			}
		case k == "format" || k == "title" || k == "description" || k == "$comment" || k == "$id" || k == "$schema" ||
			k == "contentMediaType" || k == "contentEncoding":
			if _, ok := v.(string); !ok {
				return errStrict
			}
		case k == "items":
			if list, ok := v.([]any); ok {
				for _, e := range list {
					if !isSchema(e) {
						return errStrict
					}
				}
			} else if !isSchema(v) {
				return errStrict
			}
		case contains(schemaKeys, k):
			if !isSchema(v) {
				return errStrict
			}
		case contains(schemaMapKeys, k) || k == "dependencies":
			m, ok := v.(map[string]any)
			if !ok {
				return errStrict
			}
			for _, e := range m {
				if _, isList := e.([]any); !(isSchema(e) || (k == "dependencies" && isList)) {
					return errStrict
				}
			}
		case contains(schemaArrKeys, k):
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return errStrict
			}
			for _, e := range list {
				if !isSchema(e) {
					return errStrict
				}
			}
		}
	}
	// strictTypes: declared types must fit the context, and type-specific
	// keywords need an applicable type.
	types := schemaTypesOf(s)
	if len(types) > 0 && len(ctx) > 0 {
		for _, t := range types {
			if !applicable(ctx, t) && !(t == "integer" && contains(ctx, "number")) {
				return errStrict
			}
		}
	}
	known := types
	if len(known) == 0 {
		known = ctx
	}
	for k := range s {
		if kt, ok := keywordTypes[k]; ok && !applicable(known, kt) {
			return errStrict
		}
	}
	if _, hasIf := s["if"]; !hasIf {
		if _, ok := s["then"]; ok {
			return errStrict
		}
		if _, ok := s["else"]; ok {
			return errStrict
		}
	}
	// Recurse: data-changing subschemas start without context.
	sub := func(v any, ctx []string) error {
		if m, ok := v.(map[string]any); ok {
			return strictCompile(root, m, ctx)
		}
		return nil
	}
	for _, k := range sortedKeys(s) {
		v := s[k]
		switch {
		case contains(schemaMapKeys, k) || k == "dependencies":
			m, _ := v.(map[string]any)
			for _, name := range sortedKeys(m) {
				if err := sub(m[name], nil); err != nil {
					return err
				}
			}
		case k == "items":
			if list, ok := v.([]any); ok {
				for _, e := range list {
					if err := sub(e, nil); err != nil {
						return err
					}
				}
			} else if err := sub(v, nil); err != nil {
				return err
			}
		case k == "additionalProperties" || k == "additionalItems" || k == "contains" || k == "propertyNames":
			if err := sub(v, nil); err != nil {
				return err
			}
		case k == "not" || k == "if" || k == "then" || k == "else":
			if err := sub(v, known); err != nil {
				return err
			}
		case contains(schemaArrKeys, k):
			for _, e := range v.([]any) {
				if err := sub(e, known); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// resolvable follows a local JSON pointer ref.
func resolvable(root map[string]any, ref string) bool {
	if ref == "#" {
		return true
	}
	if !strings.HasPrefix(ref, "#/") {
		return false
	}
	var cur any = root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return isSchema(cur)
}

// --- native subset ----------------------------------------------------------------

func hasValidationSiblings(s map[string]any, primary string, allowType bool) bool {
	for k := range s {
		if k != primary && !(allowType && k == "type") && !contains(annotationKeywords, k) {
			return true
		}
	}
	return false
}

func hasOnlyType(s map[string]any, want string) bool {
	var nonNull []string
	for _, t := range schemaTypesOf(s) {
		if t != "null" {
			nonNull = append(nonNull, t)
		}
	}
	return len(nonNull) == 1 && nonNull[0] == want
}

func repetitionIssue(s map[string]any, minKey, maxKey string) string {
	lo, hasLo := number(s[minKey])
	hi, hasHi := number(s[maxKey])
	if (hasLo && lo > maxNativeRepetition) || (hasHi && hi > maxNativeRepetition) {
		return fmt.Sprintf("Structured output schema %s/%s exceeds LocalBase's native repetition limit.", minKey, maxKey)
	}
	if hasLo && hasHi && lo > hi {
		return fmt.Sprintf("Structured output schema %s cannot exceed %s.", minKey, maxKey)
	}
	return ""
}

const maxSafeInteger = 1<<53 - 1

func safeInt(f float64) bool { return f == math.Trunc(f) && math.Abs(f) <= maxSafeInteger }

func literalsMatchType(s map[string]any) bool {
	var values []any
	if e, ok := s["enum"].([]any); ok {
		values = e
	} else {
		values = []any{s["const"]}
	}
	types := schemaTypesOf(s)
	for _, v := range values {
		switch v.(type) {
		case map[string]any, []any:
			return false
		}
		if _, declared := s["type"]; declared && !matchesAnyType(v, types) {
			return false
		}
	}
	return true
}

func inspectNative(s map[string]any) string {
	for _, k := range sortedKeys(s) {
		if !contains(supportedKeywords, k) && !contains(annotationKeywords, k) {
			return fmt.Sprintf("Structured output schema uses unsupported keyword '%s'.", k)
		}
	}
	if ref, present := s["$ref"]; present {
		if r, ok := ref.(string); !ok || !supportedRefRe.MatchString(r) {
			return "Structured output schema references must target direct local definitions."
		}
		if hasValidationSiblings(s, "$ref", false) {
			return "Structured output schema reference siblings are unsupported."
		}
		return ""
	}
	if _, present := s["anyOf"]; present && hasValidationSiblings(s, "anyOf", false) {
		return "Structured output schema anyOf siblings are unsupported."
	}
	_, hasEnum := s["enum"]
	_, hasConst := s["const"]
	if hasEnum || hasConst {
		if hasEnum && hasConst {
			return "Structured output schema cannot combine enum and const."
		}
		primary := "const"
		if hasEnum {
			primary = "enum"
		}
		if hasValidationSiblings(s, primary, true) {
			return fmt.Sprintf("Structured output schema %s cannot be combined with other constraints.", primary)
		}
		if !literalsMatchType(s) {
			return fmt.Sprintf("Structured output schema %s values must be scalar and satisfy the declared type.", primary)
		}
	}
	if _, present := s["format"]; present {
		return "Structured output schema formats are unsupported by the managed runtime contract."
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := s[k]; ok {
				return true
			}
		}
		return false
	}
	if has("properties", "required", "additionalProperties") && !hasOnlyType(s, "object") {
		return "Structured output object constraints require an object or nullable object type."
	}
	if has("items", "minItems", "maxItems") && !hasOnlyType(s, "array") {
		return "Structured output array constraints require an array or nullable array type."
	}
	if contains(schemaTypesOf(s), "array") {
		if _, ok := s["items"].(map[string]any); !ok {
			return "Structured output arrays require an object-valued items schema."
		}
	}
	if msg := repetitionIssue(s, "minItems", "maxItems"); msg != "" {
		return msg
	}
	if has("minLength", "maxLength") && !hasOnlyType(s, "string") {
		return "Structured output string lengths require a string or nullable string type."
	}
	if has("minLength") {
		return "Structured output schema minLength is unsupported by the managed runtime contract."
	}
	if msg := repetitionIssue(s, "minLength", "maxLength"); msg != "" {
		return msg
	}
	if has("minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum") {
		if !hasOnlyType(s, "integer") {
			return "The managed runtime supports numeric bounds only for integer schemas."
		}
		for _, k := range numberKeys {
			if f, ok := number(s[k]); ok && !safeInt(f) {
				return "Structured output integer bounds must be safe integers."
			}
		}
		if (has("minimum") && has("exclusiveMinimum")) || (has("maximum") && has("exclusiveMaximum")) {
			return "Structured output integer bounds cannot combine inclusive and exclusive limits on the same side."
		}
		lo, hasLo := number(s["minimum"])
		if !hasLo {
			if f, ok := number(s["exclusiveMinimum"]); ok {
				lo, hasLo = f+1, true
			}
		}
		hi, hasHi := number(s["maximum"])
		if !hasHi {
			if f, ok := number(s["exclusiveMaximum"]); ok {
				hi, hasHi = f-1, true
			}
		}
		if (hasLo && !safeInt(lo)) || (hasHi && !safeInt(hi)) {
			return "Structured output exclusive integer bounds must remain within the safe integer range."
		}
		if hasLo && hasHi && lo > hi {
			return "Structured output integer bounds must contain at least one value."
		}
	}
	if contains(schemaTypesOf(s), "object") {
		props, hasProps := s["properties"].(map[string]any)
		if !hasProps || s["additionalProperties"] != false {
			return "Structured output objects require properties and additionalProperties: false."
		}
		req, ok := s["required"].([]any)
		if !ok || len(req) != len(props) {
			return "Structured output objects must require every declared property."
		}
		for name := range props {
			found := false
			for _, r := range req {
				if r == name {
					found = true
				}
			}
			if !found {
				return "Structured output objects must require every declared property."
			}
		}
	}
	children, ok := childSchemas(s)
	if !ok {
		return "The managed runtime requires object-valued subschemas."
	}
	for _, c := range children {
		if msg := inspectNative(c); msg != "" {
			return msg
		}
	}
	return ""
}

// --- validation of completed responses -------------------------------------------

func matchesType(v any, t string) bool {
	switch t {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := number(v)
		return ok
	case "integer":
		f, ok := number(v)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

func matchesAnyType(v any, types []string) bool {
	for _, t := range types {
		if matchesType(v, t) {
			return true
		}
	}
	return false
}

func jsonEqual(a, b any) bool {
	fa, okA := number(a)
	fb, okB := number(b)
	if okA || okB {
		return okA && okB && fa == fb
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// validateSchema checks v against an accepted schema (the native subset).
func validateSchema(root, s map[string]any, v any) bool {
	if ref, ok := s["$ref"].(string); ok {
		parts := strings.SplitN(strings.TrimPrefix(ref, "#/"), "/", 2)
		defs, _ := root[parts[0]].(map[string]any)
		target, _ := defs[parts[1]].(map[string]any)
		return target != nil && validateSchema(root, target, v)
	}
	if any_, ok := s["anyOf"].([]any); ok {
		for _, alt := range any_ {
			if m, ok := alt.(map[string]any); ok && validateSchema(root, m, v) {
				return true
			}
		}
		return false
	}
	if _, declared := s["type"]; declared && !matchesAnyType(v, schemaTypesOf(s)) {
		return false
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, v) {
		return false
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if jsonEqual(x, v) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	switch val := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := val[r.(string)]; !present {
					return false
				}
			}
		}
		for k, x := range val {
			p, defined := props[k].(map[string]any)
			if !defined {
				if s["additionalProperties"] == false {
					return false
				}
				continue
			}
			if !validateSchema(root, p, x) {
				return false
			}
		}
	case []any:
		if lo, ok := number(s["minItems"]); ok && float64(len(val)) < lo {
			return false
		}
		if hi, ok := number(s["maxItems"]); ok && float64(len(val)) > hi {
			return false
		}
		if items, ok := s["items"].(map[string]any); ok {
			for _, x := range val {
				if !validateSchema(root, items, x) {
					return false
				}
			}
		}
	case string:
		if hi, ok := number(s["maxLength"]); ok && float64(utf8.RuneCountInString(val)) > hi {
			return false
		}
	default:
		if f, ok := number(v); ok {
			if lo, ok := number(s["minimum"]); ok && f < lo {
				return false
			}
			if hi, ok := number(s["maximum"]); ok && f > hi {
				return false
			}
			if lo, ok := number(s["exclusiveMinimum"]); ok && f <= lo {
				return false
			}
			if hi, ok := number(s["exclusiveMaximum"]); ok && f >= hi {
				return false
			}
		}
	}
	return true
}

// structuredOutcome validates a completed chat response: "passed",
// "failed" or "skipped" (refusals, tool calls, truncation and unfinished
// choices are not checked).
func structuredOutcome(resp map[string]any, schema map[string]any) string {
	choices, _ := resp["choices"].([]any)
	validated := false
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			return "failed"
		}
		if r, present := msg["refusal"]; present && r != nil {
			continue
		}
		if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
			continue
		}
		if choice["finish_reason"] != "stop" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			return "failed"
		}
		dec := json.NewDecoder(strings.NewReader(content))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil || dec.More() {
			return "failed"
		}
		if !validateSchema(schema, schema, v) {
			return "failed"
		}
		validated = true
	}
	if validated {
		return "passed"
	}
	return "skipped"
}
