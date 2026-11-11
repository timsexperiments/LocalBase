package main

import (
	"fmt"
	"strings"
	"testing"
)

// ParseActivity reads what /_localbase/host answers, whatever that is. This pins today's
// behaviour, odd corners included: a 200 whose body doesn't decode reads as "answered HTTP 200",
// only the first JSON value is read (anything after it is ignored), null and {} read as idle, and
// nothing here bounds the size: a large body is read whole (the client's 5 s timeout is the bound).
func TestParseActivity(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("x", 2<<20) + `","host":{"activity":{"busy":true}}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		busy   bool
		detail string
		err    string
	}{
		{"busy, with work", 200, `{"host":{"activity":{"busy":true,"runs":{"active":1},"llm":{"inFlight":2,"queued":1},"stt":{"inFlight":1},"video":{"queued":1}}}}`,
			true, "3 in flight, 2 queued, 1 detached runs", ""},
		{"idle", 200, `{"host":{"activity":{"busy":false}}}`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"an empty object", 200, `{}`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"null", 200, `null`, false, "0 in flight, 0 queued, 0 detached runs", ""},
		{"not a 200", 401, `{"host":{"activity":{"busy":false}}}`, false, "", "the gateway answered HTTP 401"},
		{"a redirect", 307, ``, false, "", "the gateway answered HTTP 307"},
		{"cut off", 200, `{"host":{"activity":`, false, "", "the gateway answered HTTP 200"},
		{"empty", 200, ``, false, "", "the gateway answered HTTP 200"},
		{"not JSON", 200, `<html>busy</html>`, false, "", "the gateway answered HTTP 200"},
		{"a wrong type", 200, `{"host":{"activity":{"busy":"yes"}}}`, false, "", "the gateway answered HTTP 200"},
		{"trailing bytes after the answer", 200, `{"host":{"activity":{"busy":true}}} and more`, true, "0 in flight, 0 queued, 0 detached runs", ""},
		{"a 2 MiB answer", 200, big, true, "0 in flight, 0 queued, 0 detached runs", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy, detail, err := ParseActivity(tc.status, strings.NewReader(tc.body))
			if got := fmt.Sprint(err); (tc.err == "" && err != nil) || (tc.err != "" && got != tc.err) {
				t.Errorf("error %v, want %q", err, tc.err)
			}
			if busy != tc.busy || detail != tc.detail {
				t.Errorf("%v %q, want %v %q", busy, detail, tc.busy, tc.detail)
			}
		})
	}
}
