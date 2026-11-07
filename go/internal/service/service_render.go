package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
)

var otelNames = []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG"}

// otelEnvironment passes the caller's OTEL_* settings into the service.
func otelEnvironment() map[string]string {
	env := map[string]string{}
	for _, n := range otelNames {
		if v := os.Getenv(n); v != "" {
			env[n] = v
		}
	}
	return env
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fingerprint(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ResolveInvocation is this executable serving the root.
func ResolveInvocation(rootDir string, installMissing bool) (Invocation, error) {
	exe, err := os.Executable()
	if err != nil {
		return Invocation{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return Invocation{}, err
	}
	args := []string{"--root", rootDir, "serve"}
	if installMissing {
		args = append(args, "--install-missing")
	}
	return Invocation{Program: exe, Arguments: args}, nil
}

// Definition is a rendered service definition and the manifest recording it.
type Definition struct {
	Metadata
	Contents string
	Manifest Manifest
}

func newDefinition(rootDir string, inv Invocation, token string) (Definition, error) {
	m, err := MetadataFor(rootDir)
	if err != nil {
		return Definition{}, err
	}
	otel := otelEnvironment()
	contents := RenderSystemd(m, inv, rootDir, token, otel)
	if m.Manager == "launchd" {
		contents = RenderLaunchd(m, inv, rootDir, token, otel)
	}
	bound := inv
	bound.ServiceToken = token
	return Definition{Metadata: m, Contents: contents, Manifest: Manifest{Version: 1, Root: rootDir, RootHash: root.Hash(rootDir),
		Manager: m.Manager, ServiceID: m.ServiceID, DefinitionPath: m.DefinitionPath,
		DefinitionFingerprint: fingerprint([]byte(contents)), ServiceToken: token, Invocation: bound}}, nil
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return safefile.WritePrivateChecked(path, b)
}

func (d Definition) write(rootDir string) error {
	if d.Manager == "launchd" {
		// launchd opens the stderr path itself; create it private first.
		if err := os.MkdirAll(filepath.Join(rootDir, "logs"), 0o700); err != nil {
			return err
		}
		f, err := safefile.OpenOwnedRegular(filepath.Join(rootDir, "logs", "service-stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
		if err != nil {
			return err
		}
		f.Close()
	}
	if err := atomicWrite(d.DefinitionPath, []byte(d.Contents)); err != nil {
		return err
	}
	b, _ := json.Marshal(d.Manifest)
	return atomicWrite(manifestPath(rootDir), b)
}
