package models

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzInspectVariant inspects arbitrary bytes under a file name that claims a split
// set. Plain `go test` runs the seeds; tools/fuzz.sh fuzzes on demand. Whatever the
// file says, inspection stays within its limits.
func FuzzInspectVariant(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("GGUF"))
	f.Add([]byte("GGUF\x03\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte{8, 0, 0, 0, 0, 0, 0, 0, '{', '"', 'a', '"', ':', '{', '}', '}'}) // safetensors
	f.Add([]byte("lmgg"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		name := "m-00001-of-00002.gguf"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		in := InspectVariant(variantOf(name), dir)
		if len(in.Variant.Artifacts) > maxVariantArtifacts {
			t.Fatalf("%d artifacts", len(in.Variant.Artifacts))
		}
	})
}
