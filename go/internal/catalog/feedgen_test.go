package catalog_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// The published feeds are generated from internal/catalog/data and the
// memory measurements; the checked-in copies must match byte for byte.
func TestPublishedFeedsAreCurrent(t *testing.T) {
	v1, v2, err := catalog.RenderFeeds(memory.MeasurementsJSON())
	if err != nil {
		t.Fatal(err)
	}
	root := testutil.RepoRoot(t)
	for _, c := range []struct {
		rel  string
		want []byte
	}{{"catalog/feed.json", v1}, {"catalog/feed-v2.json", v2}} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(c.rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s is out of date; run `cd go && go run ./tools/catalog-feed`", c.rel)
		}
	}
}
