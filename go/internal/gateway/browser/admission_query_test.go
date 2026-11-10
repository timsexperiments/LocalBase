package browser

import (
	"net/http"
	"testing"
)

func TestAdmissionPreviewAssumeInstalledThroughUI(t *testing.T) {
	if got := forwardedQuery("/_localbase/models/x/admission", mustGet(t, "/app/api/_localbase/models/x/admission?assumeInstalled=1&other=2")); got != "assumeInstalled=1" {
		t.Errorf("forwarded %q", got)
	}
}

func mustGet(t *testing.T, target string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", "http://127.0.0.1"+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
