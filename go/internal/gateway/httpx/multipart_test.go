package httpx

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
)

func formRequest() *http.Request {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Content-Type", gatewaytest.FormType)
	return r
}

func TestAFormUpToTheLimitIsRead(t *testing.T) {
	form, err := ReadMultipart(formRequest(), gatewaytest.FormOf(MaxFormParts))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(form["x"]); got != MaxFormParts {
		t.Errorf("%d parts were read, want %d", got, MaxFormParts)
	}
}

func TestAFormPastTheLimitIsRefused(t *testing.T) {
	if form, err := ReadMultipart(formRequest(), gatewaytest.FormOf(MaxFormParts+1)); err == nil {
		t.Errorf("it was read, with %d parts", len(form["x"]))
	}
}

func TestTheLimitIsTheStatedOne(t *testing.T) {
	if MaxFormParts != 1000 {
		t.Errorf("MaxFormParts is %d, and DESIGN says 1000", MaxFormParts)
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
	for _, size := range []int{1 << 20, MaxRequestBytes} {
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
