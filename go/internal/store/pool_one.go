//go:build poolofone

package store

// A pool of one, for TestNothingNeedsTwoConnections: anything that asks for a second connection while
// it holds one waits at once. Only a test builds with this tag; a release never does.
const (
	maxOpenConns = 1
	maxIdleConns = 1
)
