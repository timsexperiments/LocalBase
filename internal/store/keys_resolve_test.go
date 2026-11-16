package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A key is found by its own row. Another key's row that can't be read doesn't stop it working; its own
// row that can't be read still does.
func TestAKeyIsResolvedByItsOwnRowAlone(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	good, goodRaw, err := s.CreateAPIKey(ctx, "good", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	bad, badRaw, err := s.CreateAPIKey(ctx, "bad", []string{"inference:chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE api_keys SET scopes = 'not json' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}
	k, err := s.ResolveAPIKey(ctx, goodRaw)
	if err != nil || k == nil || k.ID != good.ID {
		t.Fatalf("the good key: %+v, %v", k, err)
	}
	if k, err := s.ResolveAPIKey(ctx, badRaw); err == nil || k != nil {
		t.Errorf("the key whose row can't be read: %+v, %v", k, err)
	}
	if k, err := s.ResolveAPIKey(ctx, "lb_"+strings.Repeat("A", 32)); err != nil || k != nil {
		t.Errorf("a key nobody holds: %+v, %v", k, err)
	}
}
