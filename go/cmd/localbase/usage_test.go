package main

import (
	"errors"
	"io"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// The table is buffered until the end, so this is the only place a failed write can show. Without
// it the person gets a report with no table and an exit status that says all went well.
func TestUsageTableThatCannotBeWrittenIsAnError(t *testing.T) {
	r := &usage.Report{Groups: []usage.Group{{}}}
	if err := printUsage(&cli.Ctx{Stdout: brokenWriter{}, Stderr: io.Discard}, r); err == nil {
		t.Fatal("a table nobody received should be an error")
	}
	if err := printUsage(&cli.Ctx{Stdout: io.Discard, Stderr: io.Discard}, r); err != nil {
		t.Fatalf("a table that was written: %v", err)
	}
}
