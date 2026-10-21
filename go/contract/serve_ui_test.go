package contract

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestServeForUI serves the contract fixture root (with whatever real models
// the LOCALBASE_CONTRACT_* variables link) with --no-auth on loopback, for
// checking the browser UI by hand:
//
//	LOCALBASE_CONTRACT_SERVE_UI=<port in 24000-29999> go test ./contract -run TestServeForUI -timeout 2h
//
// It stops when the stop file it prints is deleted, or after an hour.
func TestServeForUI(t *testing.T) {
	port, err := strconv.Atoi(os.Getenv("LOCALBASE_CONTRACT_SERVE_UI"))
	if err != nil {
		t.Skip("set LOCALBASE_CONTRACT_SERVE_UI to a free test port")
	}
	if port < 24000 || port > 29999 || port == 24173 || (port >= 25611 && port <= 25613) {
		t.Fatalf("port %d is outside the test range", port)
	}
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "root")
	if _, _, _, _, err := fixture(rootDir, freePort()); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goCLI(t), "--root", rootDir, "serve", "--no-auth", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--llm-port", strconv.Itoa(freePort()), "--stt-port", strconv.Itoa(freePort()), "--tts-port", strconv.Itoa(freePort()),
		"--image-port", strconv.Itoa(freePort()), "--video-port", strconv.Itoa(freePort()))
	// Discovery reads the recorded Hub (search "qwen coder"), never the network.
	cmd.Env = append(cleanEnv(), "HF_ENDPOINT="+hubFixtureServer(t, false), "HF_TOKEN=", "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost")
	logf, _ := os.Create(filepath.Join(dir, "serve.log"))
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
	os.WriteFile(stop, nil, 0o600)
	fmt.Printf("serving http://127.0.0.1:%d/app (pid %d, log %s); delete %s to stop\n", port, cmd.Process.Pid, logf.Name(), stop)
	for deadline := time.Now().Add(time.Hour); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, err := os.Stat(stop); err != nil {
			return
		}
	}
}
