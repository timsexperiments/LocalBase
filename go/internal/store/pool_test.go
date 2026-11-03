package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The store's descriptors are bounded: past the limit a request waits for a connection, and gets one.
func TestThePoolIsBounded(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.DB.Stats().MaxOpenConnections; got != MaxOpenConns {
		t.Errorf("max open %d, want %d", got, MaxOpenConns)
	}
	ctx := context.Background()
	conns := make([]*sql.Conn, MaxOpenConns)
	for i := range conns {
		if conns[i], err = st.DB.Conn(ctx); err != nil {
			t.Fatal(err)
		}
		defer conns[i].Close()
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := st.DB.Conn(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("one past the limit: %v", err)
	}
	_ = conns[0].Close()
	c, err := st.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("after one came back: %v", err)
	}
	_ = c.Close()
}
