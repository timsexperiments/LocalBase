package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
)

// `models import` prints a file's size and the total with memory.BytesText, as backup does: GiB to
// one decimal. It used two.
func TestModelsImportSaysSizesInTheOneFormat(t *testing.T) {
	t.Setenv("LOCALBASE_ROOT", "")
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	weights := filepath.Join(tmp, "sizes-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, weights, 3<<30+300<<20) // sparse: 3.29 GiB on paper
	code, out := runApp(t, "--root", root, "models", "import", weights, "--task", "chat", "--id", "sizes", "--dry-run")
	if code != 0 || !strings.Contains(out, "sizes-q4_k_m.gguf (weights, 3.3 GiB, ") || !strings.Contains(out, "Total: 3.3 GiB\n") || strings.Contains(out, "3.29") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
