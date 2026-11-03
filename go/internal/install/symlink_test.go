package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// A link at the partial path gets no downloaded bytes, and the cache file is not
// followed either.
func TestDownloadRefusesAPlantedPartial(t *testing.T) {
	data := []byte("the model's bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	spec := specFromBytes(srv.URL, data)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	content := []byte("someone's own\n")
	os.WriteFile(target, content, 0o644)
	os.Symlink(target, filepath.Join(dir, "m.bin.partial"))
	_, err := Model(context.Background(), spec, dir, Options{Available: func(string) (int64, error) { return 1 << 40, nil }})
	var unsafe *obs.UnsafeFileError
	if !errors.As(err, &unsafe) {
		t.Fatalf("want a refusal: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(content) {
		t.Errorf("the target changed: %q", got)
	}

	// A link whose target has exactly the right bytes must not become the model.
	exact := filepath.Join(t.TempDir(), "same-bytes")
	os.WriteFile(exact, data, 0o644)
	dir2 := t.TempDir()
	os.Symlink(exact, filepath.Join(dir2, "m.bin.partial"))
	if _, err := Model(context.Background(), spec, dir2, Options{Available: func(string) (int64, error) { return 1 << 40, nil }}); !errors.As(err, &unsafe) {
		t.Errorf("a link with the right bytes: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(dir2, "m.bin")); err == nil {
		t.Errorf("published: %v", info.Mode())
	}

	// A symlinked final model file with the right bytes stays allowed: weights on
	// another disk are a plausible setup, and nothing is written through it.
	dir3 := t.TempDir()
	os.Symlink(exact, filepath.Join(dir3, "m.bin"))
	if _, err := Model(context.Background(), spec, dir3, Options{Available: func(string) (int64, error) { return 1 << 40, nil }}); err != nil {
		t.Errorf("a symlinked final file: %v", err)
	}

	clean := t.TempDir()
	os.Symlink(target, filepath.Join(clean, ".checksums.json"))
	if _, err := Model(context.Background(), spec, clean, Options{Available: func(string) (int64, error) { return 1 << 40, nil }}); !errors.As(err, &unsafe) {
		t.Errorf("a linked checksum cache: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(content) {
		t.Errorf("the target changed through the cache: %q", got)
	}
}

func specFromBytes(url string, data []byte) *catalog.Spec {
	sum := sha256Hex(data)
	return &catalog.Spec{ModelID: "link-test", Kind: "stt", Source: url + "/org/repo", RepositoryRevision: "abc",
		Artifacts: []catalog.Artifact{{SourcePath: "m.bin", Filename: "m.bin", ExpectedSizeBytes: int64(len(data)), SHA256: sum, Role: "primary"}}}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
