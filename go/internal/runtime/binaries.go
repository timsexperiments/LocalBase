package runtime

//go:generate cp ../../../src/manager/managed-runtime-manifest.json managed-runtime-manifest.json

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

//go:embed managed-runtime-manifest.json
var manifestJSON []byte

type manifestArtifact struct {
	Tag               string `json:"tag"`
	AssetName         string `json:"assetName"`
	URL               string `json:"url"`
	ExpectedSizeBytes int64  `json:"expectedSizeBytes"`
	SHA256            string `json:"sha256"`
	Format            string `json:"format"`
	StripComponents   int    `json:"stripComponents"`
}

type manifest struct {
	Targets []struct {
		Platform     string                      `json:"platform"`
		Architecture string                      `json:"architecture"`
		Tier         string                      `json:"tier"`
		Runtimes     map[string]manifestArtifact `json:"runtimes"`
	} `json:"targets"`
}

// executableFamily maps an executable to the runtime package that ships it.
var executableFamily = map[string]string{
	"llama-server": "llama-server", "llama-tts": "llama-server", "whisper-server": "whisper-server",
	"sd-server": "sd-server", "sd-cli": "sd-server", "tts-server": "tts-server", "crispasr": "crispasr",
}

func pinnedRuntime(family string) (manifestArtifact, bool) {
	var m manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return manifestArtifact{}, false
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
	for _, t := range m.Targets {
		if t.Platform == goruntime.GOOS && t.Architecture == arch {
			a, ok := t.Runtimes[family]
			return a, ok
		}
	}
	return manifestArtifact{}, false
}

// ResolveBinary finds a managed executable at
// <root>/bin/runtimes/<family>/<pinned sha256>/<name>, the layout the TS
// binary installs. On CLI-only platforms a user-managed executable on PATH
// (outside <root>/bin) is used instead. Downloading arrives in milestone 2.
func ResolveBinary(runtimesDir, name string) (string, error) {
	if family, pin, _, ok := release(name); ok {
		path := filepath.Join(runtimesDir, family, strings.ToLower(pin.SHA256), name)
		if fileExists(path) {
			return path, nil
		}
		return "", fmt.Errorf("managed %s %s is not installed at %s", name, pin.Tag, filepath.Dir(path))
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH and this platform has no managed runtime", name)
	}
	if abs, _ := filepath.Abs(path); strings.HasPrefix(abs, filepath.Dir(runtimesDir)+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing unmanaged %s inside the LocalBase bin directory", name)
	}
	return path, nil
}

// HasPinnedRuntime reports a managed package for an executable on this
// platform.
func HasPinnedRuntime(name string) bool {
	_, _, _, ok := release(name)
	return ok
}

// Executable reports an existing executable regular file.
func Executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
