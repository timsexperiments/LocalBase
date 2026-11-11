package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestServeForUI serves the contract fixture root (with whatever real models
// the LOCALBASE_CONTRACT_* variables link) with --no-auth on loopback, for
// checking the browser UI by hand:
//
//	LOCALBASE_CONTRACT_SERVE_UI=<port in LOCALBASE_TEST_PORTS> go test ./contract -run TestServeForUI -timeout 2h
//
// It stops when the stop file it prints is deleted, or after an hour.
func TestServeForUI(t *testing.T) {
	port, err := strconv.Atoi(os.Getenv("LOCALBASE_CONTRACT_SERVE_UI"))
	if err != nil {
		t.Skip("set LOCALBASE_CONTRACT_SERVE_UI to a free test port")
	}
	if !testutil.PortAllowed(port) {
		t.Fatalf("port %d is outside the test block (%s)", port, testutil.PortsEnv)
	}
	dir := fsguard.ScratchDir(t)
	rootDir := filepath.Join(dir, "root")
	if _, _, _, _, err := harness.Fixture(rootDir, testutil.FreePort()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goCLI(t), "--root", rootDir, "serve", "--no-auth", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--llm-port", strconv.Itoa(testutil.FreePort()), "--stt-port", strconv.Itoa(testutil.FreePort()), "--tts-port", strconv.Itoa(testutil.FreePort()),
		"--image-port", strconv.Itoa(testutil.FreePort()), "--video-port", strconv.Itoa(testutil.FreePort()))
	// Discovery reads the recorded Hub (search "qwen coder"), never the network.
	cmd.Env = append(cleanEnv(), "HF_ENDPOINT="+hubFixtureServer(t, false), "HF_TOKEN=", "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost")
	logf, _ := fsguard.Create(filepath.Join(dir, "serve.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		cmd.Wait()
	}()
	stop := filepath.Join(dir, "stop")
	fsguard.WriteFile(stop, nil, 0o600)
	fmt.Printf("serving http://127.0.0.1:%d/app (pid %d, log %s); delete %s to stop\n", port, cmd.Process.Pid, logf.Name(), stop)
	for deadline := time.Now().Add(time.Hour); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := os.Stat(stop); err != nil {
			return
		}
	}
}
