package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLooseModelFiles: `models list` shows model files placed in a models
// directory, never dot-prefixed staging (a benchmark parts directory named
// like a GGUF, a hidden partial file) or directories.
func TestLooseModelFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("my-model.gguf")
	write("weights.safetensors")
	write(".hidden-partial.gguf")
	write(".checksums.json")
	write("notes.txt")
	os.Mkdir(filepath.Join(dir, ".benchmark-parts-Qwen3.6-35B-A3B-UD-IQ4_NL_XL.gguf"), 0o700)
	os.Mkdir(filepath.Join(dir, "folder.gguf"), 0o700)
	os.Symlink(filepath.Join(dir, "my-model.gguf"), filepath.Join(dir, "linked.gguf"))
	want := map[string]bool{"my-model.gguf": true, "weights.safetensors": true, "linked.gguf": true}
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if got := looseModelFile(dir, en.Name()); got != want[en.Name()] {
			t.Errorf("%s: listed %v, want %v", en.Name(), got, want[en.Name()])
		}
	}
}
