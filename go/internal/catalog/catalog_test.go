package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltInCatalogDecodesStrictly(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) < 10 {
		t.Fatalf("only %d models", len(c.Models))
	}
	clef := c.ByID("clef-flash-q4_k_m")
	if clef == nil || !clef.IsDecision() || !clef.Qualified() {
		t.Fatalf("clef-flash: %+v", clef)
	}
	if c.ByID("qwen3-embedding-0.6b-q8_0") == nil || !c.ByID("qwen3-embedding-0.6b-q8_0").IsEmbedding() {
		t.Fatal("embedding model not recognized")
	}
}

// The embedded copy must match src/catalog/models until TS retires.
func TestBuiltInCatalogMatchesSource(t *testing.T) {
	order, _ := builtInFS.ReadFile("data/order.txt")
	for _, rel := range strings.Fields(string(order)) {
		embedded, _ := builtInFS.ReadFile("data/" + rel)
		src, err := os.ReadFile(filepath.Join("..", "..", "..", "src", "catalog", "models", rel))
		if err != nil || !bytes.Equal(src, embedded) {
			t.Errorf("%s is out of date; run go generate ./internal/catalog", rel)
		}
	}
}

func TestUserLayer(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	// Non-launch patch keeps qualified trust.
	write("a.json", `{"modelId":"clef-flash-q4_k_m","notes":"mine"}`)
	// Launch-shaped patch makes it modified.
	write("b.json", `{"modelId":"qwen3-embedding-0.6b-q8_0","contextWindowTokens":4096}`)
	// Unknown field is rejected and reported, not fatal.
	write("c.json", `{"modelId":"clef-flash-q4_k_m","bogus":1}`)
	write("d.json", `not json`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := c.ByID("clef-flash-q4_k_m"); s.Notes != "mine" || s.Trust != "qualified" {
		t.Errorf("clef: notes=%q trust=%s", s.Notes, s.Trust)
	}
	if s := c.ByID("qwen3-embedding-0.6b-q8_0"); s.Trust != "modified" || *s.ContextWindowTokens != 4096 {
		t.Errorf("embedding trust=%s", s.Trust)
	}
	if len(c.Diagnostics) != 2 {
		t.Errorf("diagnostics = %+v", c.Diagnostics)
	}
}

func TestLiveUserCatalogDecodes(t *testing.T) {
	dir := os.Getenv("LOCALBASE_TEST_CATALOG_D")
	if dir == "" {
		t.Skip("set LOCALBASE_TEST_CATALOG_D to a copy of a catalog.d")
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Diagnostics) > 0 {
		t.Fatalf("diagnostics: %+v", c.Diagnostics)
	}
}
