package main

import (
	"strings"
	"testing"
)

func asUser(t *testing.T, uid int) {
	t.Helper()
	was := effectiveUID
	t.Cleanup(func() { effectiveUID = was })
	effectiveUID = func() int { return uid }
}

// A gateway runs the backends it downloads and reads model files from anywhere, so as root all of
// that runs as root. It stops before it binds or changes anything, unless asked not to.
func TestServeRefusesRoot(t *testing.T) {
	root := noAuthRoot(t)
	port := busyPort(t)
	asUser(t, 0)
	code, out := runApp(t, "--root", root, "serve", "--no-llm", "--port", port)
	if code == 0 || !strings.Contains(out, "Refusing to serve as root") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Refusing to serve as root") && !strings.Contains(line, "--allow-root") {
			t.Errorf("it doesn't say how to go ahead: %s", line)
		}
	}
}

// The port here is held, so getting past the check ends at the bind and not in serving.
func TestServeAsRootWhenAskedTo(t *testing.T) {
	root := noAuthRoot(t)
	port := busyPort(t)
	asUser(t, 0)
	code, out := runApp(t, "--root", root, "serve", "--no-llm", "--allow-root", "--port", port)
	if code == 0 || strings.Contains(out, "Refusing to serve as root") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}

func TestServeAsAnOrdinaryUserIsUnchanged(t *testing.T) {
	root := noAuthRoot(t)
	port := busyPort(t)
	asUser(t, 501)
	code, out := runApp(t, "--root", root, "serve", "--no-llm", "--port", port)
	if code == 0 || strings.Contains(out, "Refusing to serve as root") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}
