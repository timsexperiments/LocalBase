package web

import (
	"net/http/httptest"
	"testing"
)

// A preview can load a model, which can move another out of memory, so only this page can ask for
// one. A browser going straight to the address counts, and so does a client that sends no such
// header. Another site can't, and neither can another name under the same domain.
func TestAVoicePreviewIsForThisSiteOnly(t *testing.T) {
	for site, want := range map[string]int{"same-origin": 200, "none": 200, "": 200, "same-site": 403, "cross-site": 403, "something-new": 403} {
		api := &fakeAPI{reply: APIResult{Status: 200, Body: []byte("RIFF")}}
		u := New(Deps{NoAuth: true, Call: api.call})
		r := httptest.NewRequest("GET", "/app/lab/voices/preview?model=m&voice=v", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		w := httptest.NewRecorder()
		u.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d, want %d", site, w.Code, want)
		}
		if asked := api.path != ""; asked != (want == 200) {
			t.Errorf("%q: the speech route was asked: %v", site, asked)
		}
	}
}
