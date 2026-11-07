package runtime

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// With debug logging off, a backend's output line costs nothing beyond reading it: no string copy
// and no URL redaction for a log line nobody keeps (review 04-M5). The startup capture is full
// first, as it is after the first few kilobytes of a running backend.
func TestOutputWithDebugOffAllocatesNothingPerLine(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p := &Process{Name: "llm", startup: make([]byte, startupCapture)}
	line := "GET /v1/models?token=secret https://example.com/a/b " + strings.Repeat("x", 200) + "\n"
	input := []byte(strings.Repeat(line, 1000))
	var lines atomic.Int64
	allocs := testing.AllocsPerRun(5, func() {
		p.drainOutput(bytes.NewReader(input), &lines, log)
	})
	// The reader's buffer is per call; per line there should be nothing.
	if perLine := allocs / 1000; perLine >= 0.5 {
		t.Errorf("%.1f allocations per output line with debug off (%.0f for 1000 lines)", perLine, allocs)
	}
}
