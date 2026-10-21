package obs

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"unicode/utf16"
)

// validEvent is the TS logEventSchema: the reader keeps exactly the lines
// TS keeps, so `logs` and diagnostics see the same events from either
// binary.

var (
	uuidRe       = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)
	isoRe        = regexp.MustCompile(`^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d(?::[0-5]\d(?:\.\d+)?)?(?:Z|[+-](?:[01]\d|2[0-3]):?[0-5]\d)$`)
	componentRe  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	attrNameRe   = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	traceIDRe    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDRe     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	severities   = []string{"debug", "info", "warn", "error"}
	httpMethods  = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"}
	gatewayPaths = func() []string {
		out := append(append([]string{}, apiRoutes...), uiRoutes...)
		for _, r := range apiRoutes {
			out = append(out, "/app/api"+r)
		}
		return append(out, "unmatched-route")
	}()
)

func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

// strict decodes an object and rejects keys outside allowed.
func strict(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	for k := range m {
		if !slices.Contains(allowed, k) {
			return nil, false
		}
	}
	return m, true
}

func str(raw json.RawMessage, lo, hi int) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	n := jsLen(s)
	return s, n >= lo && n <= hi
}

func num(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func validEvent(line []byte) bool {
	m, ok := strict(line, "schemaVersion", "id", "timestamp", "severity", "eventName", "category", "component", "runtime",
		"message", "requestId", "trace", "http", "error", "attributes")
	if !ok {
		return false
	}
	if v, ok := num(m["schemaVersion"]); !ok || v != 2 {
		return false
	}
	checks := []struct {
		key      string
		lo, hi   int
		re       *regexp.Regexp
		options  []string
		optional bool
	}{
		{key: "id", lo: 1, hi: 1 << 20, re: uuidRe},
		{key: "timestamp", lo: 1, hi: 1 << 20, re: isoRe},
		{key: "severity", lo: 1, hi: 16, options: severities},
		{key: "eventName", lo: 1, hi: maxEventName, re: eventNameRe},
		{key: "category", lo: 1, hi: 16, options: categories},
		{key: "component", lo: 1, hi: maxComponent, re: componentRe},
		{key: "runtime", lo: 1, hi: 16, options: runtimes},
		{key: "message", lo: 1, hi: maxMessage},
		{key: "requestId", lo: 1, hi: maxRequestID, re: requestIDRe, optional: true},
	}
	for _, c := range checks {
		raw, present := m[c.key]
		if !present {
			if c.optional {
				continue
			}
			return false
		}
		s, ok := str(raw, c.lo, c.hi)
		if !ok || (c.re != nil && !c.re.MatchString(s)) || (c.options != nil && !slices.Contains(c.options, s)) {
			return false
		}
	}
	if raw, ok := m["trace"]; ok {
		t, ok := strict(raw, "traceId", "spanId")
		if !ok {
			return false
		}
		trace, ok1 := str(t["traceId"], 32, 32)
		span, ok2 := str(t["spanId"], 16, 16)
		if !ok1 || !ok2 || !traceIDRe.MatchString(trace) || !spanIDRe.MatchString(span) ||
			trace == "00000000000000000000000000000000" || span == "0000000000000000" {
			return false
		}
	}
	if raw, ok := m["http"]; ok {
		h, ok := strict(raw, "method", "path", "status", "durationMs")
		if !ok {
			return false
		}
		method, ok1 := str(h["method"], 1, 16)
		path, ok2 := str(h["path"], 1, 1<<10)
		status, ok3 := num(h["status"])
		duration, ok4 := num(h["durationMs"])
		if !ok1 || !ok2 || !ok3 || !ok4 || !slices.Contains(httpMethods, method) || !slices.Contains(gatewayPaths, path) ||
			status != math.Trunc(status) || status < 100 || status > 599 || duration < 0 || duration > 3_600_000 {
			return false
		}
	}
	if raw, ok := m["error"]; ok {
		e, ok := strict(raw, "type", "message", "code")
		if !ok {
			return false
		}
		_, ok1 := str(e["type"], 1, 128)
		_, ok2 := str(e["message"], 1, maxMessage)
		if !ok1 || !ok2 {
			return false
		}
		if c, present := e["code"]; present {
			if _, ok := str(c, 1, 128); !ok {
				return false
			}
		}
	}
	if raw, ok := m["attributes"]; ok {
		var attrs map[string]json.RawMessage
		if json.Unmarshal(raw, &attrs) != nil || attrs == nil || len(attrs) > maxAttributes {
			return false
		}
		for k, v := range attrs {
			if len(k) > 64 || !attrNameRe.MatchString(k) {
				return false
			}
			if _, ok := str(v, 0, maxAttributeValue); ok {
				continue
			}
			if _, ok := num(v); ok {
				continue
			}
			if t := string(bytes.TrimSpace(v)); t == "true" || t == "false" {
				continue
			}
			return false
		}
	}
	return true
}
