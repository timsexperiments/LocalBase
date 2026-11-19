package runs

import (
	"testing"

	"github.com/timsexperiments/LocalBase/internal/gateway/gatewaytest"
)

func FuzzRequestModel(f *testing.F) {
	gatewaytest.MultipartSeeds(f)
	f.Fuzz(func(t *testing.T, boundary string, body []byte) {
		_ = requestModel("multipart/form-data; boundary="+boundary, body)
	})
}
