package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// The managed runtime manifest's home is this package. Until the runtime release helpers stop
// reading src/manager (OSS's R4b, which removes this test), the sync workflows write both copies,
// and they must be identical: the binary embeds this one, the helpers verify and update that one.
func TestManagedRuntimeManifestMatchesTheHelpersCopy(t *testing.T) {
	helpers, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "src", "manager", "managed-runtime-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestJSON, helpers) {
		t.Error("src/manager/managed-runtime-manifest.json differs from go/internal/runtime/managed-runtime-manifest.json: " +
			"update both (the sync workflows copy src/manager's to go/internal/runtime)")
	}
}
