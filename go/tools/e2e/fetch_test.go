package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fetchRun runs tools/e2e.sh with a curl that does what the case says, and a cache of its own.
// Nothing here gets as far as a browser, or uses the network.
func fetchRun(t *testing.T, curl string, seedAxe bool) (cache string, cmd *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	cache = t.TempDir()
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "curl"), []byte("#!/bin/bash\n"+curl+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if seedAxe {
		pins, err := os.ReadFile("../axe.pins")
		if err != nil {
			t.Fatal(err)
		}
		version := regexp.MustCompile(`(?m)^version (\S+)$`).FindSubmatch(pins)[1]
		for _, name := range []string{".min.js", ".tgz"} {
			if err := os.WriteFile(filepath.Join(cache, "axe-core-"+string(version)+name), []byte("seeded"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cmd = exec.CommandContext(ctx, "bash", "../e2e.sh")
	cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOCALBASE_E2E_CACHE="+cache, "LOCALBASE_DISK_FLOOR_GB=0", "LBGO_CHROME=")
	output = &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = output, output
	return cache, cmd, output
}

// nothingLeft fails if a fetch left anything but what the case seeded.
func nothingLeft(t *testing.T, cache string, seeded bool) {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if seeded && strings.HasPrefix(e.Name(), "axe-core-") {
			continue
		}
		t.Errorf("left in the cache: %s", e.Name())
	}
}

func chromeIsPinnedHere(t *testing.T) {
	t.Helper()
	if !strings.Contains("darwin-arm64 linux-amd64", runtime.GOOS+"-"+runtime.GOARCH) {
		t.Skipf("no Chrome pinned for %s-%s, so this script fetches none", runtime.GOOS, runtime.GOARCH)
	}
}

// curlWrites writes half a file to whatever -o names, then ends with this status.
func curlWrites(status string) string {
	return `while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done; printf 'half of a file' > "$out"; exit ` + status
}

func TestAFailedFetchLeavesNothingAndIsNotUsed(t *testing.T) {
	for _, c := range []struct {
		name    string
		curl    string
		seedAxe bool
		says    string
	}{
		{"axe-core, the connection dropping halfway", curlWrites("18"), false, "could not fetch axe-core"},
		{"axe-core, a whole answer that is the wrong file", curlWrites("0"), false, "axe-core checksum mismatch"},
		{"Chrome, the connection dropping halfway", curlWrites("18"), true, "could not fetch Chrome"},
		{"Chrome, a whole answer that is the wrong file", curlWrites("0"), true, "Chrome checksum mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.seedAxe {
				chromeIsPinnedHere(t)
			}
			cache, cmd, output := fetchRun(t, c.curl, c.seedAxe)
			err := cmd.Run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
				t.Errorf("exit: %v, output: %s", err, output)
			}
			if !strings.Contains(output.String(), c.says) {
				t.Errorf("does not say %q: %s", c.says, output)
			}
			nothingLeft(t, cache, c.seedAxe)
		})
	}
}

func TestAStoppedFetchLeavesNothingAndStopsCurl(t *testing.T) {
	cache, cmd, output := fetchRun(t, `while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done; printf 'half of a file' > "$out"; echo $$ > "$(dirname "$out")/../curl.pid"; exec sleep 50`, false)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(cache, "curl.pid")
	var pid string
	for deadline := time.Now().Add(30 * time.Second); pid == ""; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid = strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fetch never started: %s", output)
		}
	}
	if err := os.Remove(pidFile); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Errorf("exit: %v, output: %s", err, output)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("it took %v to stop", d)
	}
	nothingLeft(t, cache, false)
	if out, _ := exec.Command("ps", "-p", pid, "-o", "pid=").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("the fetch is still running as pid %s", pid)
	}
}
