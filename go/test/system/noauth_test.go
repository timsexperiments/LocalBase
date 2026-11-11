package system

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// `serve --no-auth` starts on every loopback name and refuses anything wider.
// A Go-only rule: TS 0.1.0 has no such check.
func TestNoAuthOnlyOnLoopback(t *testing.T) {
	root := filepath.Join(fsguard.ScratchDir(t), "root")
	if res := runCLI(t, "go", root, "--json", "init"); res.code != 0 {
		t.Fatalf("init: %s", res.stderr)
	}
	args := func(host string, port int) []string {
		return []string{"--root", root, "serve", "--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video",
			"--host", host, "--port", fmt.Sprint(port)}
	}
	for _, host := range []string{"0.0.0.0", "::"} {
		cmd := exec.Command(goCLI(t), args(host, testutil.FreePort())...)
		cmd.Env = cleanEnv()
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "Refusing --no-auth") {
			t.Errorf("%s: %v\n%s", host, err, out)
		}
	}
	for _, c := range []struct{ host, url string }{{"127.0.0.1", "127.0.0.1"}, {"localhost", "localhost"}, {"::1", "[::1]"}} {
		if c.host == "::1" {
			ln, err := net.Listen("tcp", "[::1]:0")
			if err != nil {
				t.Log("no IPv6 loopback here; skipping ::1")
				continue
			}
			ln.Close()
		}
		port := testutil.FreePort()
		cmd := exec.Command(goCLI(t), args(c.host, port)...)
		cmd.Env = cleanEnv()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ok := false
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !ok; time.Sleep(100 * time.Millisecond) {
			if resp, err := http.Get(fmt.Sprintf("http://%s:%d/health", c.url, port)); err == nil {
				ok = resp.StatusCode == 200
				resp.Body.Close()
			}
		}
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
		if !ok {
			t.Errorf("%s: never served /health", c.host)
		}
	}
}
