package gateway

import (
	"maps"
	"net/http"
	"testing"
)

// What usage counts each request as now comes from the route table. These were the labels before
// the table, kept by hand; a change to one is a change to what the hourly totals call it.
func TestUsageLabelsComeFromTheRouteTable(t *testing.T) {
	want := map[string]string{
		"/v1/chat/completions":     "chat",
		"/v1/embeddings":           "embeddings",
		"/v1/systemone":            "decision",
		"/v1/audio/speech":         "speech",
		"/v1/audio/transcriptions": "transcription",
		"/v1/audio/translations":   "translation",
		"/v1/images/generations":   "image",
		"/v1/images/edits":         "image-edit",
		"/v1/videos":               "video",
	}
	if got := usageRoutes(); !maps.Equal(got, want) {
		t.Errorf("usage labels: %v, want %v", got, want)
	}
}

// The table is what the gateway registers: each pattern once, the UI's only when there is a UI, and
// the catch-all last.
func TestTheRouteTableIsWhatIsRegistered(t *testing.T) {
	for _, ui := range []bool{false, true} {
		var conf Config
		if ui {
			conf.UI = http.NotFoundHandler()
		}
		srv := New(conf)
		srv.StopRuns(0)
		table := srv.routeTable()
		if len(srv.routes) != len(table) {
			t.Fatalf("ui %v: %d registered, %d in the table", ui, len(srv.routes), len(table))
		}
		seen := map[string]bool{}
		for i, rt := range table {
			if srv.routes[i] != rt.pattern || seen[rt.pattern] {
				t.Errorf("ui %v: route %d is %q, the table says %q (or it repeats)", ui, i, srv.routes[i], rt.pattern)
			}
			seen[rt.pattern] = true
		}
		if seen["/app"] != ui || seen["/{$}"] != ui {
			t.Errorf("ui %v: the UI's routes registered %v", ui, seen["/app"])
		}
		if table[len(table)-1].pattern != "/" {
			t.Errorf("ui %v: the catch-all is not last: %q", ui, table[len(table)-1].pattern)
		}
	}
}
