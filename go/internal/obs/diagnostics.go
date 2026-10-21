package obs

import (
	"errors"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// DiagnosticsEvent re-redacts a stored event for a diagnostics bundle (TS
// redactLogEventForDiagnostics): request ids, HTTP and trace details are
// dropped, text is redacted again, and identifiers that needed redaction
// fall back to fixed names. The result keeps the TS key order.
func DiagnosticsEvent(raw []byte) (*jsjson.Object, error) {
	v, err := jsjson.Parse(raw)
	if err != nil {
		return nil, err
	}
	in, ok := v.(*jsjson.Object)
	if !ok {
		return nil, errors.New("not an event")
	}
	str := func(k string) string {
		s, _ := get(in, k).(string)
		return s
	}
	identifier := func(value, fallback string, max int) string {
		r := BoundedText(value, max)
		if strings.Contains(r, redacted) {
			return fallback
		}
		return r
	}
	out := jsjson.NewObject()
	for _, k := range []string{"schemaVersion", "id", "timestamp", "severity"} {
		out.Set(k, get(in, k))
	}
	out.Set("eventName", identifier(str("eventName"), "diagnostics.event", maxEventName))
	out.Set("category", get(in, "category"))
	out.Set("component", identifier(str("component"), "diagnostics", maxComponent))
	out.Set("runtime", get(in, "runtime"))
	out.Set("message", BoundedText(str("message"), maxMessage))
	if e, ok := get(in, "error").(*jsjson.Object); ok {
		msg, _ := get(e, "message").(string)
		out.Set("error", objectOf("message", BoundedText(msg, maxMessage)))
	}
	if attrs, ok := get(in, "attributes").(*jsjson.Object); ok {
		clean := jsjson.NewObject()
		for _, k := range attrs.Keys() {
			if clean.Len() >= maxAttributes {
				break
			}
			if key, val, ok := attribute(k, get(attrs, k)); ok {
				clean.Set(key, val)
			}
		}
		out.Set("attributes", clean)
	}
	return out, nil
}

func get(o *jsjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func objectOf(k string, v any) *jsjson.Object {
	o := jsjson.NewObject()
	o.Set(k, v)
	return o
}
