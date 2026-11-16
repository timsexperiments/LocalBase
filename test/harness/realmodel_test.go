package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

func TestRealModelGuard(t *testing.T) {
	dir := fsguard.ScratchDir(t)
	withBackend := filepath.Join(dir, "with")
	clean := filepath.Join(dir, "clean")
	fsguard.MkdirAll(withBackend, 0o700)
	fsguard.MkdirAll(clean, 0o700)
	fsguard.WriteFile(filepath.Join(withBackend, "llama-server"), []byte("#!/bin/sh\n"), 0o700)
	t.Setenv("PATH", withBackend+string(os.PathListSeparator)+clean)
	t.Setenv("HTTPS_PROXY", "http://real.example:1")
	t.Setenv(allowRealModelsEnv, "")

	check := func(env []string) {
		t.Helper()
		value := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			value[k] = v // the last wins, as for a process
		}
		if value["PATH"] != clean {
			t.Errorf("PATH: %q", value["PATH"])
		}
		if value["HTTPS_PROXY"] != "http://127.0.0.1:9" || value["NO_PROXY"] != "127.0.0.1,localhost" {
			t.Errorf("network: %v %v", value["HTTPS_PROXY"], value["NO_PROXY"])
		}
	}
	check(Guarded(os.Environ())) // what every launched binary gets (go/contract checked it through its CLI env, cleanEnv)

	// Only an explicit setting brings real backends back.
	t.Setenv(allowRealModelsEnv, "1")
	if got := WithoutRealBackends(os.Getenv("PATH")); !strings.Contains(got, withBackend) {
		t.Errorf("with %s: %q", allowRealModelsEnv, got)
	}
}
