package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"
)

// Install states of a backend on this device.
const (
	Installed   = "installed"   // the executable is present
	Installable = "installable" // a pinned managed package exists; LocalBase installs it on first use
	Missing     = "missing"     // nothing to run, and no managed package
)

// Host is what detection knows about this device.
type Host struct {
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Accelerators []string `json:"accelerators"`
	// Install maps a backend id to Installed, Installable or Missing.
	Install map[string]string `json:"install"`
	// Unpackaged marks built-in backends with no managed package for this
	// platform (nothing LocalBase could install).
	Unpackaged map[string]bool `json:"unpackaged,omitempty"`
	// Unpinned marks built-in backends whose runtime LocalBase manages but
	// has no pinned package for on this device, installed or not: they are
	// never picked automatically here (CrispASR is automatic only where
	// pinned).
	Unpinned map[string]bool `json:"unpinned,omitempty"`
}

// Has reports an accelerator.
func (h Host) Has(acc string) bool { return slices.Contains(h.Accelerators, acc) }

// ProbeOptions inject the commands and files the probe reads (tests).
type ProbeOptions struct {
	Run    func(name string, args ...string) (string, error)
	Exists func(path string) bool
}

func runCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ProbeAccelerators finds GPUs without loading any GPU library: Metal on
// Apple Silicon, CUDA when nvidia-smi lists a GPU, Vulkan when a loader and
// an ICD are installed. CPU is always present.
func ProbeAccelerators(o ProbeOptions) []string {
	if o.Run == nil {
		o.Run = runCommand
	}
	if o.Exists == nil {
		o.Exists = exists
	}
	accs := []string{}
	switch goruntime.GOOS {
	case "darwin":
		if goruntime.GOARCH == "arm64" {
			accs = append(accs, "metal")
		}
	case "linux":
		if out, err := o.Run("nvidia-smi", "-L"); err == nil && strings.Contains(out, "GPU ") {
			accs = append(accs, "cuda")
		}
		loader := o.Exists("/usr/lib/x86_64-linux-gnu/libvulkan.so.1") || o.Exists("/usr/lib64/libvulkan.so.1") ||
			o.Exists("/usr/lib/libvulkan.so.1") || o.Exists("/usr/lib/aarch64-linux-gnu/libvulkan.so.1")
		icd := false
		for _, d := range []string{"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d"} {
			if m, _ := filepath.Glob(filepath.Join(d, "*.json")); len(m) > 0 {
				icd = true
			}
		}
		if loader && icd {
			accs = append(accs, "vulkan")
		}
	}
	return append(accs, "cpu")
}

// GoArch is the manifest architecture name of this build.
func GoArch() string {
	return map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
}

// NewHost describes this device. install reports each backend's install
// state (the caller knows where runtimes live).
func NewHost(accelerators []string, reg *Registry, install func(*Definition) string, packaged func(*Definition) bool) Host {
	h := Host{OS: goruntime.GOOS, Arch: GoArch(), Accelerators: accelerators, Install: map[string]string{}, Unpackaged: map[string]bool{},
		Unpinned: map[string]bool{}}
	for _, d := range reg.All() {
		h.Install[d.ID] = install(d)
		if d.BuiltIn && d.Launch == nil && d.Runtime != nil && d.Runtime.Managed && packaged != nil && !packaged(d) {
			h.Unpinned[d.ID] = true
		}
		if d.BuiltIn && d.Launch == nil && h.Install[d.ID] == Missing && packaged != nil && !packaged(d) {
			h.Unpackaged[d.ID] = true
		}
	}
	return h
}

// DeviceID identifies this machine for per-device choices: a hash of the
// platform UUID (IOPlatformUUID, /etc/machine-id), never the raw value. A
// root copied to another machine therefore starts in automatic mode there.
// LOCALBASE_DEVICE_ID overrides it (tests, and machines without an id).
func DeviceID(o ProbeOptions) string {
	if v := os.Getenv("LOCALBASE_DEVICE_ID"); v != "" {
		return v
	}
	if o.Run == nil {
		o.Run = runCommand
	}
	raw := ""
	switch goruntime.GOOS {
	case "darwin":
		out, _ := o.Run("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `"IOPlatformUUID"`) {
				if i := strings.LastIndex(line, "="); i >= 0 {
					raw = strings.Trim(strings.TrimSpace(line[i+1:]), `"`)
				}
			}
		}
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
				raw = strings.TrimSpace(string(b))
				break
			}
		}
	}
	if raw == "" {
		host, _ := os.Hostname()
		raw = "hostname:" + host
	}
	sum := sha256.Sum256([]byte("localbase-device:" + raw))
	return hex.EncodeToString(sum[:16])
}

// InstallLabel is the install state in words, for lists and selectors.
func (h Host) InstallLabel(id string) string {
	switch h.Install[id] {
	case Installed:
		return "installed"
	case Installable:
		return "installs on first use"
	}
	if h.Unpackaged[id] {
		return "not installed (no managed package yet)"
	}
	return "not installed"
}
