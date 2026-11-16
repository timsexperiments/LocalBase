package jsjson

import "testing"

// A document is one value, as JSON.parse reads it. Whatever follows it is refused unless it is whitespace,
// whether or not it is JSON itself.
func TestOnlyWhitespaceMayFollowTheDocument(t *testing.T) {
	for _, in := range []string{`{"a":1} garbage`, `{"a":1} }`, `[1,2] ]`, `{"a":1}x`, `{"a":1} {"b":2}`, `1 2`, `"s" ,`, `null null`} {
		if _, err := Parse([]byte(in)); err == nil || err.Error() != "trailing data" {
			t.Errorf("%q: %v", in, err)
		}
	}
	for _, in := range []string{`{"a":1}`, "{\"a\":1}  \n\t\r\n", ` [1,2] `, `"s"`, `0`} {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}
