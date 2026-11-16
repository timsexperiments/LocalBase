package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/netguard"
)

// A catalog entry can come from a remote feed, so its source reaches public
// addresses only unless the person has opted in.
func TestInstallKeepsToPublicAddresses(t *testing.T) {
	data := []byte("model bytes")
	sum := sha256.Sum256(data)
	var reached atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Write(data)
	}))
	defer srv.Close()
	spec := &catalog.Spec{ModelID: "m", Kind: "stt", Source: srv.URL + "/org/m", RepositoryRevision: "rev",
		Artifacts: []catalog.Artifact{{SourcePath: "m.bin", Filename: "m.bin", ExpectedSizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Role: "primary"}}}
	free := func(string) (int64, error) { return 1 << 40, nil }
	_, err := Model(context.Background(), spec, t.TempDir(), Options{Available: free})
	if !netguard.IsBlocked(err) || reached.Load() != 0 {
		t.Fatalf("%v, reached %d", err, reached.Load())
	}
	if _, err := Model(context.Background(), spec, t.TempDir(), Options{Available: free, AllowPrivate: true}); err != nil || reached.Load() != 1 {
		t.Fatalf("with the opt-in: %v, reached %d", err, reached.Load())
	}
}
