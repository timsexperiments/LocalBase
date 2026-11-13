package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
)

// serve's output is what it was: under --json, a started line (with the first-run link when there is
// one) and a stopped line on stdout; otherwise the address and the link for a person, and nothing at
// the stop.
func TestServeReportsWhatItDid(t *testing.T) {
	var out, errOut bytes.Buffer
	rep := serveReporter{&cli.Ctx{Global: cli.Global{JSON: true}, Stdout: &out, Stderr: &errOut}}
	rep.Started("http://127.0.0.1:2273", map[string]bool{"llm": true, "stt": false}, "http://127.0.0.1:2273/x#t")
	rep.Started("http://127.0.0.1:2273", map[string]bool{"llm": true}, "")
	rep.Stopped()
	want := `{"baseUrl":"http://127.0.0.1:2273","enabled":{"llm":true,"stt":false},"event":"started","loginLink":"http://127.0.0.1:2273/x#t"}
{"baseUrl":"http://127.0.0.1:2273","enabled":{"llm":true},"event":"started"}
{"event":"stopped","exitCode":0}
`
	if out.String() != want || errOut.Len() != 0 {
		t.Errorf("--json: stdout\n%s\nstderr\n%s", out.String(), errOut.String())
	}

	out.Reset()
	rep = serveReporter{&cli.Ctx{Stdout: &out, Stderr: &errOut}}
	rep.Started("http://127.0.0.1:2273", map[string]bool{"llm": true}, "http://127.0.0.1:2273/x#t")
	rep.Stopped()
	text := out.String()
	if !strings.HasPrefix(text, "LocalBase gateway listening on http://127.0.0.1:2273\n\nSign in from a browser on this computer.") ||
		!strings.Contains(text, "\n  http://127.0.0.1:2273/x#t\n") || strings.Contains(text, "stopped") || errOut.Len() != 0 {
		t.Errorf("text: stdout\n%s\nstderr\n%s", text, errOut.String())
	}
}
