package gateway

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
)

// The server span joins a trace a caller started: an incoming traceparent is its parent, so the
// span carries the caller's trace id.
func TestTheServerSpanFollowsAnIncomingTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	c := newSpanCollector(t)
	ts := newTestServerConfigured(t, func(conf *Config) { conf.Otel = c.rt })
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	id, _ := hex.DecodeString(traceID)
	if out := c.exported(); !bytes.Contains(out, id) {
		t.Errorf("the server span isn't in the caller's trace %s", traceID)
	}
}

// The page's frequent poll (uiPollPath) has no server span, so it doesn't fill a collector. A
// gateway that answered only the poll exports no span at all; the same request to another path
// does export one, so the collector can tell.
func TestThePagePollHasNoSpan(t *testing.T) {
	poll := newSpanCollector(t)
	ts := newTestServerConfigured(t, func(conf *Config) { conf.Otel = poll.rt })
	get(t, ts.URL+uiPollPath)
	if out := poll.exported(); bytes.Contains(out, []byte("http.route")) {
		t.Errorf("the page poll %s has a span", uiPollPath)
	}
	other := newSpanCollector(t)
	ts = newTestServerConfigured(t, func(conf *Config) { conf.Otel = other.rt })
	get(t, ts.URL+"/app/strip-not-the-poll")
	if out := other.exported(); !bytes.Contains(out, []byte("http.route")) {
		t.Error("a request beside the poll has no span, so this can't tell")
	}
}
