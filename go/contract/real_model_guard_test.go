package contract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// allowRealModelsEnv lets a run see real backends on PATH. Real runs otherwise go
// through LOCALBASE_CONTRACT_RUNTIMES, which copies a package into the scratch root.
const allowRealModelsEnv = "LOCALBASE_CONTRACT_ALLOW_REAL_MODELS"

var realBackends = []string{"llama-server", "sd-server", "whisper-server", "tts-server"}

// offlineEnv is what every launched binary gets: downloads fail fast, loopback
// (the fake servers) still works. Nothing can fetch a runtime or a model.
var offlineEnv = []string{"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
	"https_proxy=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9",
	"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"}

// withoutRealBackends drops PATH directories holding a real backend, unless
// allowRealModelsEnv is set, so a test cannot start one by accident.
func withoutRealBackends(path string) string {
	if os.Getenv(allowRealModelsEnv) == "1" {
		return path
	}
	var keep []string
	for _, dir := range filepath.SplitList(path) {
		if !slices.ContainsFunc(realBackends, func(name string) bool {
			info, err := os.Stat(filepath.Join(dir, name))
			return err == nil && !info.IsDir()
		}) {
			keep = append(keep, dir)
		}
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// guarded applies the guard to an environment: the last PATH wins.
func guarded(env []string) []string {
	out := make([]string, 0, len(env)+len(offlineEnv)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasSuffix(strings.ToLower(name), "_proxy") && name != "PATH" {
			out = append(out, kv)
		}
	}
	out = append(out, offlineEnv...)
	return append(out, "PATH="+withoutRealBackends(os.Getenv("PATH")))
}

func TestRealModelGuard(t *testing.T) {
	dir := scratchDir(t)
	withBackend := filepath.Join(dir, "with")
	clean := filepath.Join(dir, "clean")
	mustMkdirAll(withBackend, 0o700)
	mustMkdirAll(clean, 0o700)
	mustWriteFile(filepath.Join(withBackend, "llama-server"), []byte("#!/bin/sh\n"), 0o700)
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
	check(cleanEnv())

	// Only an explicit setting brings real backends back.
	t.Setenv(allowRealModelsEnv, "1")
	if got := withoutRealBackends(os.Getenv("PATH")); !strings.Contains(got, withBackend) {
		t.Errorf("with %s: %q", allowRealModelsEnv, got)
	}
}
