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

// The page's frequent poll (uiPollPath) has no server span, so it doesn't fill a collector, while
// the request beside it does.
func TestThePagePollHasNoSpan(t *testing.T) {
	c := newSpanCollector(t)
	ts := newTestServerConfigured(t, func(conf *Config) { conf.Otel = c.rt })
	get(t, ts.URL+uiPollPath)
	get(t, ts.URL+"/v1/models")
	out := c.exported()
	if !bytes.Contains(out, []byte("GET /v1/models")) {
		t.Fatal("no span for the request beside the poll, so this can't tell")
	}
	if bytes.Contains(out, []byte(uiPollPath)) {
		t.Errorf("the page poll %s has a span", uiPollPath)
	}
}
