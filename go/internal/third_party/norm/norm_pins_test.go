package norm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestPinnedCopy fails when this copy drifts from PINS, and checks the
// upstream hashes against the running toolchain when it vendors the same
// x/text version.
func TestPinnedCopy(t *testing.T) {
	b, err := os.ReadFile("PINS")
	if err != nil {
		t.Fatal(err)
	}
	var module string
	upstream, local := map[string]string{}, map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if m, ok := strings.CutPrefix(line, "# module: "); ok {
			module = strings.Fields(m)[0] + " " + strings.Fields(m)[1]
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		switch f[0] {
		case "upstream":
			upstream[f[1]] = f[2]
		case "local":
			local[f[1]] = f[2]
		}
	}
	if len(local) == 0 || len(upstream) == 0 || module == "" {
		t.Fatal("PINS is incomplete")
	}
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			if _, ok := local[n]; !ok {
				t.Errorf("%s is not pinned", n)
			}
		}
	}
	for name, want := range local {
		if got := fileSHA(t, name); got != want {
			t.Errorf("%s changed (sha256 %s); update PINS and the README if this is intended", name, got)
		}
	}
	root := filepath.Join(goruntime.GOROOT(), "src", "vendor")
	mods, err := os.ReadFile(filepath.Join(root, "modules.txt"))
	if err != nil || !strings.Contains(string(mods), "# "+module+"\n") {
		t.Logf("toolchain %s does not vendor %s; upstream hashes not rechecked", goruntime.Version(), module)
		return
	}
	for name, want := range upstream {
		p := filepath.Join(root, "golang.org", "x", "text", "unicode", "norm", name)
		if name == "LICENSE" {
			p = filepath.Join(root, "golang.org", "x", "text", "LICENSE")
		}
		if got := fileSHA(t, p); got != want {
			t.Errorf("upstream %s differs from the pinned copy of %s", name, module)
		}
	}
}
