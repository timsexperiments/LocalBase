package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// The real pinned inputs (downloaded once into /tmp by hand) reproduce the
// screen's dictionary; small inputs pin the JavaScript semantics.
func TestMergeMisaki(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "g.json"), []byte(`{"b":"gold-b","10":{"DEFAULT":"x","NOUN":null},"a":"q\"u\\o\u0001te é","2":"two"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "s.json"), []byte(`{"a":"silver-a","c":"silver-c","1":"one","b":"silver-b"}`), 0o644)
	got, err := MergeMisaki(filepath.Join(dir, "g.json"), filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Same as: const o = {...g}; for (k of Object.keys(s)) if (!(k in o)) o[k] = s[k]; JSON.stringify(o) + "\n"
	want := `{"1":"one","2":"two","10":{"DEFAULT":"x","NOUN":null},"b":"gold-b","a":"q\"u\\o\u0001te é","c":"silver-c"}` + "\n"
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	gold, silver := "/tmp/lbgo-crisp/misaki/us_gold.json", "/tmp/lbgo-crisp/misaki/us_silver.json"
	if _, err := os.Stat(gold); err != nil {
		t.Skip("pinned misaki inputs not downloaded")
	}
	b, err := MergeMisaki(gold, silver)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if h := hex.EncodeToString(sum[:]); h != "9c44a4b8661fa600f0019c0d4996dc3899e08bcaf978b06a8b816821a70e6c62" {
		t.Fatalf("merged dictionary sha256 %s", h)
	}
}
