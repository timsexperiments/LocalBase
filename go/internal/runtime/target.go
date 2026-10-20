package runtime

import (
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/memory"

	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// Target is a model resolved through this device's backend selection: the
// backend that runs it, the variant and where its files are.
type Target struct {
	ModelID string
	Model   *models.Model
	Variant *models.Variant
	Backend *backends.Definition
	Dir     string // directory holding the variant's files
	// Manual: the person chose this backend (or the model was imported), so
	// startup errors are reported as that backend's own errors.
	Manual   bool
	Imported bool
}

// Gate resolves a model for launch. It returns ErrModelNotFound for an
// unknown id and a *LaunchError when the selection cannot launch.
type Gate func(modelID string) (*Target, error)

// LaunchError is a selection problem found before anything starts.
type LaunchError struct {
	Code    string // backend_not_selected | backend_unavailable | model_assets_missing | backend_task_unsupported
	Message string
}

func (e *LaunchError) Error() string { return e.Message }

// BackendStartError is a startup failure of a backend the person chose or
// an imported model; its cause is reported instead of a generic outage.
type BackendStartError struct {
	BackendID string
	Cause     error
}

func (e *BackendStartError) Error() string {
	return fmt.Sprintf("backend %s failed to start: %v", e.BackendID, e.Cause)
}

func (e *BackendStartError) Unwrap() error { return ErrStartupFailed }

// weights returns the variant's first weights file (shard 1 of a split set).
func (t *Target) weights() (string, error) {
	var first string
	for _, a := range t.Variant.Artifacts {
		if a.Role == models.RoleWeights && (first == "" || a.Filename < first) {
			first = a.Filename
		}
	}
	if first == "" {
		return "", errors.New("the variant has no weights file")
	}
	return first, nil
}

func (t *Target) projector() string {
	for _, a := range t.Variant.Artifacts {
		if a.Role == models.RoleProjector {
			return a.Filename
		}
	}
	return ""
}

func (t *Target) task() string {
	for _, task := range []string{"embeddings", "decision", "chat"} {
		if t.Model.HasTask(task) {
			return task
		}
	}
	return ""
}

// launchCommand returns the process name, executable, argv and health path
// for the selected backend. Every adapter keeps the same admission, health
// and process supervision; only the command differs.
func (m *LLM) launchCommand(plan *LLMPlan, t *Target) (name, bin string, args []string, health string, err error) {
	darwinArm64 := goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64"
	if t == nil {
		bin := m.d.Binary
		if bin == "" {
			if bin, err = ResolveBinary(m.d.RuntimesDir, "llama-server"); err != nil {
				return "", "", nil, "", err
			}
		}
		return "llama-server", bin, plan.Args(darwinArm64), "/health", nil
	}
	d := t.Backend
	if !runsHere(d) {
		return "", "", nil, "", fmt.Errorf("%s has no build for %s/%s", d.Name, goruntime.GOOS, goruntime.GOARCH)
	}
	switch d.Adapter {
	case "llama-server/v1":
		args = plan.Args(darwinArm64)
		accelerators := map[string]bool{}
		for _, p := range d.Platforms {
			accelerators[p.Accelerator] = true
		}
		if len(accelerators) == 1 && accelerators["cpu"] {
			args = append(args, "-ngl", "0") // the chosen CPU build: no layers offloaded
		}
		switch {
		case d.Launch != nil && d.Launch.Command != "":
			bin = d.Launch.Command // a custom llama-server build
		case m.d.Binary != "":
			bin = m.d.Binary
		case d.Runtime != nil:
			if bin, err = ResolveBinary(m.d.RuntimesDir, d.Runtime.Executable); err != nil {
				return "", "", nil, "", fmt.Errorf("%s is not installed on this device: %v", d.Name, err)
			}
		default:
			return "", "", nil, "", fmt.Errorf("%s has no executable", d.Name)
		}
		if d.Runtime != nil && !d.Runtime.Managed && (d.Launch == nil || d.Launch.Command == "") && m.d.Binary == "" &&
			!(len(accelerators) == 1 && accelerators["cpu"]) {
			return "", "", nil, "", fmt.Errorf("%s has no managed package for this platform; register a build of it as a custom backend", d.Name)
		}
		return "llama-server", bin, args, "/health", nil
	case "process-openai/v1":
		l := d.Launch
		repl := strings.NewReplacer("{model}", plan.ModelPath, "{model_dir}", filepath.Dir(plan.ModelPath),
			"{projector}", plan.ProjectorPath, "{host}", plan.Host, "{port}", strconv.Itoa(plan.Port), "{ctx}", strconv.Itoa(plan.CtxSize))
		for _, a := range l.Args {
			args = append(args, repl.Replace(a))
		}
		health = l.HealthPath
		if health == "" {
			health = "/v1/models"
		}
		return d.ID, l.Command, args, health, nil
	}
	return "", "", nil, "", fmt.Errorf("%s (adapter %s) does not run %s models", d.Name, d.Adapter, t.task())
}

// customDemand is a custom backend's declared memory rule. It is an
// estimate whose assumption the definition states.
func customDemand(t *Target, plan *LLMPlan) memory.Demand {
	mem := t.Backend.Launch.Memory
	b := int64(float64(plan.ArtifactBytes)*mem.WeightsFactor) + int64(mem.OverheadGB*float64(memory.GiB))
	return memory.Demand{UnifiedBytes: b, HostBytes: b, AcceleratorBytes: b, Confidence: "declared"}
}

// runsHere reports a platform entry for this OS and architecture. Whether
// its accelerator exists is a detection finding; the backend reports a
// missing device itself when it starts.
func runsHere(d *backends.Definition) bool {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goruntime.GOARCH]
	for _, p := range d.Platforms {
		if (p.OS == goruntime.GOOS || p.OS == "*") && (p.Arch == arch || p.Arch == "*") {
			return true
		}
	}
	return false
}
