package otlp

import (
	"encoding/hex"
	"math"
)

// Decoding turns an export request into plain values for tests and
// collectors: maps keyed by the proto JSON field names, attributes as a
// key → value map, ids as hex, kinds and status codes as numbers.

type schema map[int]struct {
	name string
	kind string // msg:<schema>, str, bytes, uint, int, fixed64, fixed32, double, bool
	rep  bool
}

var schemas map[string]schema

func init() {
	kv := schema{1: {"key", "str", false}, 2: {"value", "msg:any", false}}
	schemas = map[string]schema{
		"traces":   {1: {"resourceSpans", "msg:resourceSpans", true}},
		"logs":     {1: {"resourceLogs", "msg:resourceLogs", true}},
		"resource": {1: {"attributes", "msg:kv", true}, 2: {"droppedAttributesCount", "uint", false}},
		"scope": {1: {"name", "str", false}, 2: {"version", "str", false}, 3: {"attributes", "msg:kv", true},
			4: {"droppedAttributesCount", "uint", false}},
		"resourceSpans": {1: {"resource", "msg:resource", false}, 2: {"scopeSpans", "msg:scopeSpans", true}, 3: {"schemaUrl", "str", false}},
		"scopeSpans":    {1: {"scope", "msg:scope", false}, 2: {"spans", "msg:span", true}, 3: {"schemaUrl", "str", false}},
		"span": {1: {"traceId", "bytes", false}, 2: {"spanId", "bytes", false}, 3: {"traceState", "str", false},
			4: {"parentSpanId", "bytes", false}, 5: {"name", "str", false}, 6: {"kind", "uint", false},
			7: {"startTimeUnixNano", "fixed64", false}, 8: {"endTimeUnixNano", "fixed64", false},
			9: {"attributes", "msg:kv", true}, 10: {"droppedAttributesCount", "uint", false}, 11: {"events", "msg:event", true},
			12: {"droppedEventsCount", "uint", false}, 13: {"links", "msg:link", true}, 14: {"droppedLinksCount", "uint", false},
			15: {"status", "msg:status", false}, 16: {"flags", "fixed32", false}},
		"event": {1: {"timeUnixNano", "fixed64", false}, 2: {"name", "str", false}, 3: {"attributes", "msg:kv", true},
			4: {"droppedAttributesCount", "uint", false}},
		"link":         {1: {"traceId", "bytes", false}, 2: {"spanId", "bytes", false}, 4: {"attributes", "msg:kv", true}},
		"status":       {2: {"message", "str", false}, 3: {"code", "uint", false}},
		"resourceLogs": {1: {"resource", "msg:resource", false}, 2: {"scopeLogs", "msg:scopeLogs", true}, 3: {"schemaUrl", "str", false}},
		"scopeLogs":    {1: {"scope", "msg:scope", false}, 2: {"logRecords", "msg:logRecord", true}, 3: {"schemaUrl", "str", false}},
		"logRecord": {1: {"timeUnixNano", "fixed64", false}, 11: {"observedTimeUnixNano", "fixed64", false},
			2: {"severityNumber", "uint", false}, 3: {"severityText", "str", false}, 5: {"body", "msg:any", false},
			6: {"attributes", "msg:kv", true}, 7: {"droppedAttributesCount", "uint", false}, 8: {"flags", "fixed32", false},
			9: {"traceId", "bytes", false}, 10: {"spanId", "bytes", false}, 12: {"eventName", "str", false}},
		"kv":     kv,
		"array":  {1: {"values", "msg:any", true}},
		"kvlist": {1: {"values", "msg:kv", true}},
		"any": {1: {"stringValue", "str", false}, 2: {"boolValue", "bool", false}, 3: {"intValue", "int", false},
			4: {"doubleValue", "double", false}, 5: {"arrayValue", "msg:array", false}, 6: {"kvlistValue", "msg:kvlist", false},
			7: {"bytesValue", "bytes", false}},
	}
}

// DecodeTraces decodes an ExportTraceServiceRequest.
func DecodeTraces(b []byte) (map[string]any, error) { return decode(b, "traces") }

// DecodeLogs decodes an ExportLogsServiceRequest.
func DecodeLogs(b []byte) (map[string]any, error) { return decode(b, "logs") }

func decode(b []byte, name string) (map[string]any, error) {
	fields, err := Fields(b)
	if err != nil {
		return nil, err
	}
	s := schemas[name]
	out := map[string]any{}
	for _, f := range fields {
		d, ok := s[f.Num]
		if !ok {
			out["unknown_"+string(rune('0'+f.Num))] = true
			continue
		}
		var v any
		switch {
		case len(d.kind) > 4 && d.kind[:4] == "msg:":
			m, err := decode(f.Bytes, d.kind[4:])
			if err != nil {
				return nil, err
			}
			v = m
		case d.kind == "str":
			v = string(f.Bytes)
		case d.kind == "bytes":
			v = hex.EncodeToString(f.Bytes)
		case d.kind == "double":
			v = math.Float64frombits(f.Value)
		case d.kind == "bool":
			v = f.Value != 0
		case d.kind == "int":
			v = float64(int64(f.Value))
		default:
			v = float64(f.Value)
		}
		if d.rep {
			list, _ := out[d.name].([]any)
			out[d.name] = append(list, v)
		} else {
			out[d.name] = v
		}
	}
	// Attributes as a map: key → plain value.
	if list, ok := out["attributes"].([]any); ok {
		attrs := map[string]any{}
		for _, e := range list {
			m := e.(map[string]any)
			attrs[m["key"].(string)] = Plain(m["value"])
		}
		out["attributes"] = attrs
	}
	return out, nil
}

// Plain is an AnyValue's value: string, bool, float64 (int and double,
// tagged by Number), []any or map[string]any.
func Plain(v any) any {
	m, _ := v.(map[string]any)
	for k, x := range m {
		switch k {
		case "intValue":
			return Number{x.(float64), true}
		case "doubleValue":
			return Number{x.(float64), false}
		case "arrayValue":
			var out []any
			vals, _ := x.(map[string]any)["values"].([]any)
			for _, e := range vals {
				out = append(out, Plain(e))
			}
			return out
		case "kvlistValue":
			return x.(map[string]any)["attributes"]
		default:
			return x
		}
	}
	return nil
}

// Number keeps whether an attribute was written as an int or a double.
type Number struct {
	Value float64
	Int   bool
}
