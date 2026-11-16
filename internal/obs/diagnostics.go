package obs

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// A bundle exists to be shared, so it doesn't say who it came from. These attributes name a person,
// what they called a key, or where they keep their files. The log on their own computer keeps them.
var identifyingAttributes = []string{"root", "auth_principal_id", "auth_principal_name", "actor_id", "target_id"}

var (
	homeDirRe = regexp.MustCompile(`(/Users|/home)/[^/\s"':]+`)
	addressRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
)

// withoutIdentity takes a person's home directory and any address out of text. Anything could be
// quoted in an error, so this goes over all of it.
func withoutIdentity(s string) string {
	s = homeDirRe.ReplaceAllString(s, "$1/<someone>")
	return addressRe.ReplaceAllString(s, "<address>")
}

// DiagnosticsEvent re-redacts a stored event for a diagnostics bundle (TS
// redactLogEventForDiagnostics): request ids, HTTP and trace details are
// dropped, text is redacted again, and identifiers that needed redaction
// fall back to fixed names. Anything saying who it came from goes too. The
// result keeps the TS key order.
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
	out.Set("message", withoutIdentity(BoundedText(str("message"), maxMessage)))
	if e, ok := get(in, "error").(*jsjson.Object); ok {
		msg, _ := get(e, "message").(string)
		out.Set("error", objectOf("message", withoutIdentity(BoundedText(msg, maxMessage))))
	}
	if attrs, ok := get(in, "attributes").(*jsjson.Object); ok {
		clean := jsjson.NewObject()
		for _, k := range attrs.Keys() {
			if clean.Len() >= maxAttributes {
				break
			}
			if slices.Contains(identifyingAttributes, strings.ToLower(k)) {
				continue
			}
			if key, val, ok := attribute(k, get(attrs, k)); ok {
				if text, isText := val.(string); isText {
					val = withoutIdentity(text)
				}
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
