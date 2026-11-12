//go:build !poolofone

package store_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Code that holds a connection, in a transaction or with rows open, and asks for another can wait for
// ever once the pool is full. A pool of one shows it at once, as a test that runs out of time. This
// runs those packages again built that way, the gateway with every package under it. It only sees
// paths their tests reach.
func TestNothingNeedsTwoConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs other packages' tests again")
	}
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// These hold a connection on purpose, to stand in for a writer or to fill the pool.
	const skip = "TestPolicyLookupDoesNotTakeTheWriteLock"
	// A real gateway process, built with the same tag through GOFLAGS.
	gatewayTests := []string{"TestLimitedChangesCannotPushOutWhoChangedKeys", "TestRefusedChangesCannotPushOutWhoChangedWhat", "TestInstallCancel"}
	for _, r := range []struct {
		run []string
		// tests must each run and pass. With none named, each package in run must pass a test
		// (a pattern ending in "/..." needs one package under it). A run that selects nothing
		// passes too, so without this a moved test or package would leave it checking nothing.
		tests []string
	}{
		{[]string{"-skip", skip, "./internal/auth", "./internal/store", "./internal/gateway/..."}, nil},
		{[]string{"-run", strings.Join(gatewayTests, "|"), "./test/system"}, gatewayTests},
	} {
		run := append([]string{"test", "-json", "-count=1", "-timeout=240s"}, r.run...)
		var packages []string
		if r.tests == nil {
			packages = slices.DeleteFunc(slices.Clone(r.run), func(a string) bool { return !strings.HasPrefix(a, "./") })
		}
		cmd := exec.Command("go", run...)
		cmd.Dir = moduleRoot
		cmd.Env = append(os.Environ(), "GOFLAGS=-tags=poolofone")
		started := time.Now()
		out, err := cmd.CombinedOutput()
		t.Logf("%s: %v", strings.Join(run[len(run)-1:], " "), time.Since(started).Round(time.Second))
		if err != nil {
			text := plainOutput(out)
			if i := strings.Index(text, "panic: test timed out"); i >= 0 {
				text = text[i:min(len(text), i+600)]
			} else if len(text) > 3000 {
				text = text[len(text)-3000:]
			}
			t.Errorf("with a pool of one: %v\n%s", err, text)
			continue
		}
		for _, missing := range notRun(out, packages, r.tests) {
			t.Errorf("with a pool of one, %s: %s", strings.Join(run[len(run)-1:], " "), missing)
		}
	}
}

// plainOutput is a `go test -json` stream as go test would have printed it: each event's output,
// and any line that isn't an event (a build failure's) as it is.
func plainOutput(stream []byte) string {
	var b strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e struct{ Output *string }
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			b.WriteString(sc.Text() + "\n")
		} else if e.Output != nil {
			b.WriteString(*e.Output)
		}
	}
	return b.String()
}

// notRun reads a `go test -json` stream and says which of packages ran no test that passed (a
// pattern ending in "/..." is satisfied by any package under it) and which of tests didn't pass,
// as top-level tests in any package.
func notRun(stream []byte, packages, tests []string) []string {
	passedIn := map[string]bool{} // package import path -> a test passed in it
	passed := map[string]bool{}   // top-level test name -> passed
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e struct{ Action, Package, Test string }
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Action != "pass" || e.Test == "" {
			continue
		}
		passedIn[e.Package] = true
		if !strings.Contains(e.Test, "/") {
			passed[e.Test] = true
		}
	}
	var missing []string
	for _, p := range packages {
		dir, tree := strings.CutSuffix(strings.TrimPrefix(p, "."), "/...")
		found := false
		for pkg := range passedIn {
			found = found || strings.HasSuffix(pkg, dir) || tree && strings.Contains(pkg, dir+"/")
		}
		if !found {
			missing = append(missing, "no test passed in "+p)
		}
	}
	for _, name := range tests {
		if !passed[name] {
			missing = append(missing, name+" did not run and pass")
		}
	}
	return missing
}
