package httpx

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

func FuzzMultipart(f *testing.F) {
	gatewaytest.MultipartSeeds(f)
	f.Fuzz(func(t *testing.T, boundary string, body []byte) {
		ctype := "multipart/form-data; boundary=" + boundary
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.Header.Set("Content-Type", ctype)
		_, _ = ReadMultipart(r, body)
	})
}
