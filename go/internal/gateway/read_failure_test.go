package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A model list with memory figures can't go on without what was seen of them: it says which read
// failed and to try again, in the usual error shape, and doesn't list figures with no peaks.
func TestAFailedPeakReadIsNotAListWithoutPeaks(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	s := runServer(ts)
	get := func(path string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Retry-After")
	}
	paths := []string{"/v1/models", "/v1/models/" + chatModel, "/_localbase/models", "/_localbase/models/" + chatModel}
	for _, path := range paths {
		if code, body, _ := get(path); code != 200 {
			t.Fatalf("%s, before: %d %.200s", path, code, body)
		}
	}
	if _, err := s.Store.DB.Exec(`DROP TABLE model_observed_memory`); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		code, body, retry := get(path)
		if code != 503 || retry != "5" {
			t.Errorf("%s: %d, Retry-After %q: %.200s", path, code, retry, body)
		}
		if !strings.Contains(body, `"code":"service_unavailable"`) || !strings.Contains(body, "Could not finish reading the memory peaks seen for ") {
			t.Errorf("%s: %s", path, body)
		}
		if strings.Contains(body, "no such table") {
			t.Errorf("%s shows the raw error: %s", path, body)
		}
	}
}
