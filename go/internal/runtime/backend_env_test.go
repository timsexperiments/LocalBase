package runtime

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// What a backend sees, by asking a program to print it, both when a caller gives no environment
// and when it builds one from the gateway's.
func TestABackendDoesNotGetTheGatewaysCredentials(t *testing.T) {
	t.Setenv("LOCALBASE_API_KEY", "planted-key")
	t.Setenv("HF_TOKEN", "planted-token")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "planted-headers")
	t.Setenv("KEPT_FOR_THE_BACKEND", "kept-value")
	for name, env := range map[string][]string{
		"inherited": nil,
		"built":     {"LOCALBASE_SERVICE_TOKEN=planted-service", "HF_TOKEN=planted-token", "KEPT_FOR_THE_BACKEND=kept-value"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			p, err := StartProcess("env-check", "/usr/bin/env", nil, env, log)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.Done():
			case <-time.After(60 * time.Second):
				t.Fatal("it never ended")
			}
			got := logs.String()
			if strings.Contains(got, "planted-") {
				t.Errorf("a credential got through: %s", got)
			}
			if !strings.Contains(got, "kept-value") {
				t.Errorf("what it needs didn't get through: %s", got)
			}
		})
	}
}

// envDumper is a program that writes what it was given to a file.
func envDumper(t *testing.T) (bin, out string) {
	t.Helper()
	dir := t.TempDir()
	bin, out = filepath.Join(dir, "dump"), filepath.Join(dir, "env.txt")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n/usr/bin/env > \"$ENV_DUMP\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_DUMP", out)
	t.Setenv("HF_TOKEN", "planted-token")
	t.Setenv("LOCALBASE_API_KEY", "planted-key")
	return bin, out
}

func checkEnvDump(t *testing.T, out string) {
	t.Helper()
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the program didn't run: %v", err)
	}
	if strings.Contains(string(b), "planted-") {
		t.Errorf("a credential got through: %s", b)
	}
	if !strings.Contains(string(b), "ENV_DUMP=") {
		t.Errorf("what it needs didn't get through: %s", b)
	}
}

// The short questions the gateway asks a backend's program get the same treatment.
func TestAskingABackendsProgramKeepsCredentialsBack(t *testing.T) {
	t.Run("the image contract", func(t *testing.T) {
		bin, out := envDumper(t)
		_ = requireSdGPUContract(bin)
		checkEnvDump(t, out)
	})
	t.Run("a flag in the help", func(t *testing.T) {
		bin, out := envDumper(t)
		(&HostPlacement{}).hasFlag(bin, "--anything")
		checkEnvDump(t, out)
	})
}
