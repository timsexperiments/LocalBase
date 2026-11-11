package chaos

import (
	"fmt"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// Bursts of mixed reads and writes fill the connection pool, and its descriptors level off under
// the ceiling, however many bursts come. Everything else stays where it was.
func TestDatabaseDescriptorsLevelOff(t *testing.T) {
	ctx, g, c := start(t, lab.Options{})
	first := settledStats(t, ctx, g, c)
	most := first.Files.DB
	for burst := range 8 {
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%4 == 0 {
					c.PostJSON(ctx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{"action": "create", "name": fmt.Sprintf("burst-%d-%d", burst, i), "scopes": []string{"models:read"}})
				} else {
					c.Do(ctx, "GET", "/v1/models", g.Keys.User, "", nil)
				}
			}()
		}
		wg.Wait()
		s := settledStats(t, ctx, g, c)
		t.Logf("after burst %d: %d descriptors, db %d, -wal %d, -shm %d", burst+1, s.FDs, s.Files.DB, s.Files.WAL, s.Files.SHM)
		if problem := lab.DescriptorProblem(first, s, 6, lab.DatabaseCeiling); problem != "" {
			t.Errorf("after burst %d: %s", burst+1, problem)
		}
		most = max(most, s.Files.DB)
	}
	// A ceiling nothing came near would say nothing.
	if store.MaxOpenConns > store.MaxIdleConns && most <= store.MaxIdleConns {
		t.Errorf("the bursts only reached %d descriptors on the database, so they never filled the pool past its %d idle connections", most, store.MaxIdleConns)
	}
}
