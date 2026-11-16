package chaos

import (
	"errors"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// The check can fail: each way of not keeping the promise is planted. No gateway here.
func TestBackWithinHoldsToWhatTheAnswerPromised(t *testing.T) {
	ok := lab.Call{Status: 200}
	again := lab.Call{Status: 503, Code: "service_unavailable", RetryAfter: "1"}
	first := lab.Call{Status: 503, Code: "service_unavailable", RetryAfter: "1"}
	script := func(calls ...lab.Call) func() lab.Call {
		i := 0
		return func() lab.Call {
			r := calls[min(i, len(calls)-1)]
			i++
			return r
		}
	}
	for _, c := range []struct {
		name  string
		first lab.Call
		ask   func() lab.Call
		want  string
	}{
		{"back at once", first, script(ok), ""},
		{"a clean 503 first, then back", first, script(again, again, ok), ""},
		{"never back", first, script(again), "not back within the 1 s"},
		{"a 502 on the way", first, script(lab.Call{Status: 502, Code: "upstream_error"}, ok), "on the way back: 502"},
		{"a 503 with no Retry-After on the way", first, script(lab.Call{Status: 503, Code: "x"}, ok), "on the way back: 503"},
		{"a connection error on the way", first, script(lab.Call{Err: errors.New("refused")}, ok), "on the way back"},
		{"no promise to hold it to", lab.Call{Status: 503, Code: "x"}, script(ok), "not a number of seconds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := backWithin(c.first, c.ask)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
