package acquire

import (
	"net/http"
	"testing"
)

// A profile's own Authorization header is never replaced by the Hugging Face token, whatever case the
// profile writes its name in.
func TestAProfilesAuthorizationIsKeptOverTheToken(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		t.Run(name, func(t *testing.T) {
			var sent string
			p := &Profile{ID: "org", Origins: []string{"https://huggingface.co"}, Headers: map[string]string{name: "Bearer profile-credential"}}
			tr := &scopedTransport{profile: p, hfToken: "hf_planted_token", hfOrigs: []string{"https://huggingface.co"},
				base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					sent = r.Header.Get("Authorization")
					return noContent(r), nil
				})}
			req, err := http.NewRequest(http.MethodGet, "https://huggingface.co/org/repo/resolve/main/m.gguf", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if sent != "Bearer profile-credential" {
				t.Errorf("sent %q", sent)
			}
		})
	}
}
