package otlp

import (
	"context"
	"net/http"
	"testing"
)

// Every request can carry these headers, with or without a credential. Plain `go test` runs the
// seeds, and tools/fuzz.sh fuzzes on demand.
func FuzzExtract(f *testing.F) {
	f.Add("00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "a=b")
	f.Add("ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "")
	f.Add("00-00000000000000000000000000000000-0000000000000000-00", ",,,=")
	f.Add("", "")
	f.Add("01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-extra", "k=v,k=w")
	f.Fuzz(func(t *testing.T, traceparent, tracestate string) {
		h := http.Header{}
		h.Set("Traceparent", traceparent)
		h.Set("Tracestate", tracestate)
		Extract(context.Background(), h)
	})
}
