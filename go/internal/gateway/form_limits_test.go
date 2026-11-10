package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// Each route that takes a form refuses one past the limit, and the same form within the limit goes
// on to that route's own checks, so it's the count being refused.
func TestEveryRouteRefusesAFormPastTheLimit(t *testing.T) {
	g := newMatrixGateway(t, nil)
	_, key, err := g.store.CreateAPIKey(context.Background(), "all", []string{"inference:transcription", "inference:image", "models:manage"})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + key, "Content-Type": gatewaytest.FormType}
	for _, c := range []struct{ name, path, refusal string }{
		{"transcription", "/v1/audio/transcriptions", "Invalid form data payload."},
		{"image edit", "/v1/images/edits", "Invalid multipart image edit request."},
		{"voice", "/_localbase/voices", "Invalid form data payload."},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := g.raw(t, http.MethodPost, c.path, "", headers, string(gatewaytest.FormOf(httpx.MaxFormParts+1)))
			if r.status != 400 || !strings.Contains(r.body, c.refusal) {
				t.Errorf("past the limit: %d %.300s", r.status, r.body)
			}
			r = g.raw(t, http.MethodPost, c.path, "", headers, string(gatewaytest.FormOf(httpx.MaxFormParts)))
			if strings.Contains(r.body, c.refusal) {
				t.Errorf("at the limit: %d %.300s", r.status, r.body)
			}
		})
	}
	// A gateway without a video runtime never gets as far as the form.
	t.Run("video", func(t *testing.T) {
		send := func(parts int) (int, string) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/videos", bytes.NewReader(gatewaytest.FormOf(parts)))
			r.Header.Set("Content-Type", gatewaytest.FormType)
			g.srv.readVideoCreate(w, r)
			var answer struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(w.Body.Bytes(), &answer)
			return w.Code, answer.Error.Message
		}
		const refusal = "Invalid multipart video request."
		if code, message := send(httpx.MaxFormParts + 1); code != 400 || message != refusal {
			t.Errorf("past the limit: %d %q", code, message)
		}
		if code, message := send(httpx.MaxFormParts); message == refusal {
			t.Errorf("at the limit: %d %q", code, message)
		}
	})
}
