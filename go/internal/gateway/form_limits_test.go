package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

func formRequest() *http.Request {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Content-Type", gatewaytest.FormType)
	return r
}

func TestAFormUpToTheLimitIsRead(t *testing.T) {
	form, err := ReadMultipart(formRequest(), gatewaytest.FormOf(httpx.MaxFormParts))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(form["x"]); got != httpx.MaxFormParts {
		t.Errorf("%d parts were read, want %d", got, httpx.MaxFormParts)
	}
}

func TestAFormPastTheLimitIsRefused(t *testing.T) {
	if form, err := ReadMultipart(formRequest(), gatewaytest.FormOf(httpx.MaxFormParts+1)); err == nil {
		t.Errorf("it was read, with %d parts", len(form["x"]))
	}
}

func TestTheLimitIsTheStatedOne(t *testing.T) {
	if httpx.MaxFormParts != 1000 {
		t.Errorf("MaxFormParts is %d, and DESIGN says 1000", httpx.MaxFormParts)
	}
}

// Parts cost memory however little is in them, and a form at the body limit holds millions of the
// smallest. It starts small, because without the limit the largest costs gigabytes to try.
func TestAFormAtTheBodyLimitCostsLittle(t *testing.T) {
	const bound = 16 << 20
	allocated := func(f func()) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	smallest := "--b\r\n\r\n\r\n"
	for _, size := range []int{1 << 20, httpx.MaxRequestBytes} {
		body := []byte(strings.Repeat(smallest, size/len(smallest)))
		cost := allocated(func() {
			if _, err := ReadMultipart(formRequest(), body); err == nil {
				t.Errorf("%d bytes of parts were read", size)
			}
		})
		if cost > bound {
			t.Fatalf("reading %d bytes of parts allocated %d bytes", size, cost)
		}
	}
}

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
