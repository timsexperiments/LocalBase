package conf

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
)

// The configuration document is a file a person edits. Plain `go test` runs the seeds, and
// tools/fuzz.sh fuzzes on demand.
func FuzzParseDocument(f *testing.F) {
	cat, err := catalog.Load("")
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{
		"", "a = 1", "[gateway]\nport = 1\n", "[[x]]\n[[x]]\n", "key = \"\\u0000\"", "[a.b.c]\nd = [1, [2, [3]]]\n", "= 1", "[", "a = {b = {c = 1}}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) { _, _ = ParseDocument(text, cat) })
}
