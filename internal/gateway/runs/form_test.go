package runs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

func TestAFormsModelIsFoundWithinTheLimit(t *testing.T) {
	model := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nthe-model\r\n"
	if got := requestModel(gatewaytest.FormType, gatewaytest.FormOf(httpx.MaxFormParts-1, model)); got != "the-model" {
		t.Errorf("the last part allowed: %q", got)
	}
	if got := requestModel(gatewaytest.FormType, gatewaytest.FormOf(httpx.MaxFormParts, model)); got != "" {
		t.Errorf("past the limit: %q", got)
	}
}

// Finding a form's model costs as little, on the same forms.
func TestFindingAFormsModelAtTheBodyLimitCostsLittle(t *testing.T) {
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
		cost := allocated(func() { requestModel(gatewaytest.FormType, body) })
		if cost > bound {
			t.Fatalf("finding the model in %d bytes of parts allocated %d bytes", size, cost)
		}
	}
}
