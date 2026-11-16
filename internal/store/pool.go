//go:build !poolofone

package store

// Each open connection holds the database and its -wal open, so this bounds the descriptors the store
// can use, at about twice MaxOpenConns. A request past it waits in the pool's queue, which it would
// otherwise do in SQLite's, where a writer can lose for the whole busy timeout.
const (
	MaxOpenConns = 16
	MaxIdleConns = 4
)
