package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
func WithoutRealBackends(path string) string {
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
func Guarded(env []string) []string {
	out := make([]string, 0, len(env)+len(offlineEnv)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasSuffix(strings.ToLower(name), "_proxy") && name != "PATH" {
			out = append(out, kv)
		}
	}
	out = append(out, offlineEnv...)
	return append(out, "PATH="+WithoutRealBackends(os.Getenv("PATH")))
}
