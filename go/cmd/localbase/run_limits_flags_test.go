package main

import (
	"strings"
	"testing"
)

func TestRunMemoryFlagsAreInHelpAndChecked(t *testing.T) {
	code, out := runApp(t, "serve", "--help")
	if code != 0 {
		t.Fatalf("%d: %s", code, out)
	}
	for _, want := range []string{"--run-memory-limit-mib", "--runs-memory-limit-mib"} {
		if !strings.Contains(out, want) {
			t.Errorf("serve --help lacks %s", want)
		}
		if code, out := runApp(t, "--root", t.TempDir(), "serve", want, "0"); code == 0 || !strings.Contains(out, want+" must be from 1") {
			t.Errorf("%s 0: %d %s", want, code, out)
		}
	}
}
