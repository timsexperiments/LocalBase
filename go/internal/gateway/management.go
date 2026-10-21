package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// /_localbase/model-management (TS model-management.ts): the catalog with
// install state, and install, uninstall, enable, disable and activate. One
// install runs at a time in the background; GET reports its progress.
// "active" means the persisted default, not a resident runtime.

type mgmtOperation struct {
	Action          string `json:"action"`
	State           string `json:"state"` // running | complete | failed
	Detail          string `json:"detail"`
	DownloadedBytes *int64 `json:"downloadedBytes"`
	TotalBytes      *int64 `json:"totalBytes"`
}

type mgmtEntry struct {
	ID                       string         `json:"id"`
	Installed                bool           `json:"installed"`
	InstalledBytes           int64          `json:"installedBytes"`
	DownloadBytes            *int64         `json:"downloadBytes"`
	Enabled                  bool           `json:"enabled"`
	Active                   bool           `json:"active"`
	CanInstall               bool           `json:"canInstall"`
	InstallUnavailableReason *string        `json:"installUnavailableReason"`
	Operation                *mgmtOperation `json:"operation"`
}

type mgmtStorage struct {
	AvailableBytes *int64 `json:"availableBytes"`
	TotalBytes     *int64 `json:"totalBytes"`
}

// mgmtError is a rejection before an operation is accepted.
type mgmtError struct{ code, message string }

func (e *mgmtError) Error() string { return e.message }

type management struct {
	mu         sync.Mutex
	operations map[string]*mgmtOperation
	installing string
	inFlight   sync.WaitGroup
}

func i64(v int64) *int64 { return &v }

// safeFile rejects every symlink below the canonical root, including
// directory components. It returns the file's info, or nil when missing.
func safeFile(rootDir, path string) (os.FileInfo, error) {
	canonical, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	suffix, err := filepath.Rel(canonical, abs)
	if err != nil || suffix == "." || suffix == ".." || strings.HasPrefix(suffix, ".."+string(filepath.Separator)) || filepath.IsAbs(suffix) {
		return nil, &mgmtError{"unsafe_path", "Artifact must remain inside the model root."}
	}
	parts := strings.Split(suffix, string(filepath.Separator))
	current := canonical
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		last := i == len(parts)-1
		if info.Mode()&os.ModeSymlink != 0 || (!last && !info.IsDir()) || (last && !info.Mode().IsRegular()) {
			return nil, &mgmtError{"unsafe_path", "Artifact path contains a symlink or a non-file target."}
		}
		if last {
			return info, nil
		}
	}
	return nil, nil
}

// storageAt is the file system space at path or its nearest existing parent.
func storageAt(path string) mgmtStorage {
	for current := path; ; current = filepath.Dir(current) {
		var st syscall.Statfs_t
		err := syscall.Statfs(current, &st)
		if err == nil {
			return mgmtStorage{i64(int64(st.Bavail) * int64(st.Bsize)), i64(int64(st.Blocks) * int64(st.Bsize))}
		}
		if !errors.Is(err, syscall.ENOENT) || filepath.Dir(current) == current {
			return mgmtStorage{}
		}
	}
}

func (s *Server) modelsDir(kind string) string { return s.Root.ModelsDir(kind) }

func (s *Server) artifactPaths(spec *catalog.Spec) []string {
	out := make([]string, 0, len(spec.Artifacts))
	for _, a := range spec.Artifacts {
		out = append(out, filepath.Join(s.modelsDir(spec.Kind), a.Filename))
	}
	return out
}

var sha256Re = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

type mgmtFacts struct {
	installed      bool
	installedBytes int64
	downloadBytes  *int64
}

func (s *Server) inspectModel(spec *catalog.Spec) (mgmtFacts, error) {
	f := mgmtFacts{installed: true}
	authoritative := len(spec.Artifacts) > 0
	var total int64
	for _, a := range spec.Artifacts {
		path := filepath.Join(s.modelsDir(spec.Kind), a.Filename)
		info, err := safeFile(s.Root.Dir, path)
		if err != nil {
			return f, err
		}
		partial, err := safeFile(s.Root.Dir, path+".partial")
		if err != nil {
			return f, err
		}
		if info != nil {
			f.installedBytes += info.Size()
		}
		if partial != nil {
			f.installedBytes += partial.Size()
		}
		f.installed = f.installed && info != nil && info.Size() == a.ExpectedSizeBytes
		if a.ExpectedSizeBytes <= 0 || !sha256Re.MatchString(a.SHA256) {
			authoritative = false
		} else {
			total += a.ExpectedSizeBytes
		}
	}
	if authoritative {
		f.downloadBytes = i64(total)
	}
	return f, nil
}

// referencedModels are the models a runtime holds or is switching to.
func (s *Server) referencedModels() map[string]bool {
	out := map[string]bool{}
	if s.LLM != nil {
		if id := s.LLM.Snapshot().ModelID; id != "" {
			out[id] = true
		}
	}
	for _, svc := range []*runtime.Service{s.STT, s.TTS, s.Image} {
		if svc != nil {
			if id := svc.Running(); id != "" {
				out[id] = true
			}
		}
	}
	return out
}

func (s *Server) installConflict(cfg *store.Config, spec *catalog.Spec, referenced map[string]bool) string {
	targets := map[string]bool{}
	for _, p := range s.artifactPaths(spec) {
		targets[p] = true
	}
	for _, other := range s.Catalog.All() {
		overlaps := other.ModelID == spec.ModelID
		if !overlaps {
			for _, p := range s.artifactPaths(other) {
				if targets[p] {
					overlaps = true
					break
				}
			}
		}
		if !overlaps {
			continue
		}
		if slices.Contains(cfg.Selected[other.Kind], other.ModelID) || cfg.Active[other.Kind] == other.ModelID || referenced[other.ModelID] {
			if other.ModelID == spec.ModelID {
				return "Disable this model and wait for the runtime to release it before installing."
			}
			return "Disable " + other.ModelID + " and wait for its runtime to release shared artifacts before installing."
		}
	}
	return ""
}

func (s *Server) readManagement() (mgmtStorage, []mgmtEntry) {
	cfg := s.LLM.Config()
	referenced := s.referencedModels()
	m := s.mgmt
	m.mu.Lock()
	installing := m.installing
	ops := map[string]mgmtOperation{}
	for k, v := range m.operations {
		ops[k] = *v
	}
	m.mu.Unlock()
	entries := []mgmtEntry{}
	for _, spec := range s.Catalog.All() {
		if spec.QualificationState == "experimental" && !cfg.AllowExperimental {
			continue
		}
		var reason string
		facts, err := s.inspectModel(spec)
		if err != nil {
			facts = mgmtFacts{} // as TS: not installed, nothing counted
		} else {
			_, err = safeFile(s.Root.Dir, filepath.Join(s.modelsDir(spec.Kind), ".checksums.json"))
		}
		if err != nil {
			reason = "Could not inspect model storage."
			var me *mgmtError
			if errors.As(err, &me) {
				reason = me.message
			}
		}
		available := storageAt(s.modelsDir(spec.Kind)).AvailableBytes
		eligibility := conf.Ineligible(spec, cfg.AllowExperimental)
		conflict := s.installConflict(cfg, spec, referenced)
		switch {
		case eligibility != "":
			reason = eligibility
		case installing != "":
			reason = "Wait for the current installation to finish."
		case conflict != "":
			reason = conflict
		case reason == "" && facts.downloadBytes == nil:
			reason = "Catalog model lacks authoritative sizes or SHA-256 hashes."
		case reason == "" && available == nil:
			reason = "Could not determine available storage."
		case reason == "" && *available < *facts.downloadBytes:
			reason = "Insufficient storage for model installation."
		}
		e := mgmtEntry{ID: spec.ModelID, Installed: facts.installed, InstalledBytes: facts.installedBytes, DownloadBytes: facts.downloadBytes,
			Enabled: slices.Contains(cfg.Selected[spec.Kind], spec.ModelID), Active: cfg.Active[spec.Kind] == spec.ModelID,
			CanInstall: reason == ""}
		if reason != "" {
			e.InstallUnavailableReason = &reason
		}
		if op, ok := ops[spec.ModelID]; ok {
			e.Operation = &op
		}
		entries = append(entries, e)
	}
	return storageAt(s.Root.Dir), entries
}

// updateConfig applies a change to the stored configuration under the root
// operation lease and hands it to the runtimes.
func (s *Server) updateConfig(fn func(*store.Config) error) error {
	return root.WithOperation(s.Root.Dir, "update runtime configuration", func() error {
		cur, err := s.Store.LoadConfig(context.Background())
		if err != nil {
			return err
		}
		next := cur.Clone()
		if err := fn(next); err != nil {
			return err
		}
		if err := s.Store.SaveConfig(context.Background(), next); err != nil {
			return err
		}
		s.LLM.SetConfig(next)
		return nil
	})
}

func (s *Server) runManagement(modelID, action string) (*mgmtOperation, error) {
	spec := s.Catalog.ByID(modelID)
	if spec == nil {
		return nil, &mgmtError{"invalid_request", "Unknown catalog model."}
	}
	m := s.mgmt
	m.mu.Lock()
	busy := m.installing != ""
	m.mu.Unlock()
	if busy {
		return nil, &mgmtError{"conflict", "Wait for the current installation to finish."}
	}
	cfg := s.LLM.Config()
	kind := spec.Kind
	if action == "install" || action == "enable" || action == "activate" {
		if reason := conf.Ineligible(spec, cfg.AllowExperimental); reason != "" {
			return nil, &mgmtError{"invalid_request", reason}
		}
	}
	done := &mgmtOperation{Action: action, State: "complete", Detail: "Complete"}
	switch action {
	case "install":
		return s.startInstall(spec)
	case "uninstall":
		err := root.WithOperation(s.Root.Dir, "uninstall model", func() error {
			fresh, err := s.Store.LoadConfig(context.Background())
			if err != nil {
				return err
			}
			if slices.Contains(fresh.Selected[kind], modelID) || fresh.Active[kind] == modelID || s.referencedModels()[modelID] {
				return &mgmtError{"conflict", "Disable the model and wait for runtime reconciliation before uninstalling."}
			}
			shared := map[string]bool{}
			for _, other := range s.Catalog.All() {
				if other.ModelID == modelID {
					continue
				}
				for _, p := range s.artifactPaths(other) {
					shared[p], shared[p+".partial"] = true, true
				}
			}
			var present []string
			for _, p := range s.artifactPaths(spec) {
				for _, t := range []string{p, p + ".partial"} {
					if shared[t] || slices.Contains(present, t) {
						continue
					}
					info, err := safeFile(s.Root.Dir, t)
					if err != nil {
						return err
					}
					if info != nil {
						present = append(present, t)
					}
				}
			}
			for _, p := range present {
				if err := os.Remove(p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	default:
		err := s.updateConfig(func(c *store.Config) error {
			selected := c.Selected[kind]
			if action == "enable" || action == "activate" {
				f, err := s.inspectModel(spec)
				if err != nil {
					return err
				}
				if !f.installed {
					return &mgmtError{"conflict", "Install the model first."}
				}
			}
			switch action {
			case "activate":
				if !slices.Contains(selected, modelID) {
					return &mgmtError{"conflict", "Enable the model before activating it."}
				}
				c.Active[kind] = modelID
			case "enable":
				if !slices.Contains(selected, modelID) {
					c.Selected[kind] = append(selected, modelID)
				}
			case "disable":
				if c.Active[kind] == modelID {
					if kind == "llm" {
						return &mgmtError{"conflict", "Activate another LLM before disabling the default."}
					}
					c.Active[kind] = ""
				}
				c.Selected[kind] = slices.DeleteFunc(slices.Clone(selected), func(id string) bool { return id == modelID })
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	m.operations[modelID] = done
	m.mu.Unlock()
	return done, nil
}

// startInstall runs the pre-checks under the root operation lease, then
// downloads in the background; it returns once the install is accepted.
func (s *Server) startInstall(spec *catalog.Spec) (*mgmtOperation, error) {
	m := s.mgmt
	m.mu.Lock()
	m.installing = spec.ModelID
	m.mu.Unlock()
	clear := func() {
		m.mu.Lock()
		m.installing = ""
		m.mu.Unlock()
	}
	admitted := make(chan error, 1)
	running := &mgmtOperation{Action: "install", State: "running", Detail: "Preparing installation", DownloadedBytes: i64(0)}
	m.inFlight.Add(1)
	go func() {
		defer m.inFlight.Done()
		accepted := false
		err := root.WithOperation(s.Root.Dir, "install model", func() error {
			fresh, err := s.Store.LoadConfig(context.Background())
			if err != nil {
				return err
			}
			dir := s.modelsDir(spec.Kind)
			if _, err := safeFile(s.Root.Dir, filepath.Join(dir, ".checksums.json")); err != nil {
				return err
			}
			if c := s.installConflict(fresh, spec, s.referencedModels()); c != "" {
				return &mgmtError{"conflict", c}
			}
			facts, err := s.inspectModel(spec)
			if err != nil {
				return err
			}
			if facts.downloadBytes == nil {
				return &mgmtError{"invalid_request", "Catalog model lacks authoritative sizes or SHA-256 hashes."}
			}
			available := storageAt(dir).AvailableBytes
			if available == nil {
				return &mgmtError{"storage_unavailable", "Could not determine available storage."}
			}
			// Reserve the full download even for present artifacts: verification may replace them.
			if *available < *facts.downloadBytes {
				return &mgmtError{"insufficient_storage", "Insufficient storage for model installation."}
			}
			running.TotalBytes = facts.downloadBytes
			m.mu.Lock()
			snapshot := *running
			m.operations[spec.ModelID] = &snapshot
			m.mu.Unlock()
			accepted = true
			admitted <- nil
			progress := map[string]int64{}
			_, err = install.Model(context.Background(), spec, dir, install.Options{HFToken: fresh.HFToken, Report: func(ev install.Event) {
				switch ev.Kind {
				case "download-progress":
					progress[ev.Filename] = ev.Downloaded
				case "verification-completed":
					for _, a := range spec.Artifacts {
						if a.Filename == ev.Filename {
							progress[ev.Filename] = a.ExpectedSizeBytes
						}
					}
				}
				var sum int64
				for _, v := range progress {
					sum += v
				}
				op := *running
				op.Detail, op.DownloadedBytes = ev.Kind, i64(sum)
				m.mu.Lock()
				m.operations[spec.ModelID] = &op
				m.mu.Unlock()
			}})
			return err
		})
		m.mu.Lock()
		switch {
		case err == nil:
			op := *running
			op.State, op.Detail, op.DownloadedBytes = "complete", "Installation complete", running.TotalBytes
			m.operations[spec.ModelID] = &op
		case accepted:
			op := *m.operations[spec.ModelID]
			op.State, op.Detail = "failed", "Installation failed. Check storage, network access, and catalog verification."
			m.operations[spec.ModelID] = &op
		}
		m.installing = ""
		m.mu.Unlock()
		if !accepted {
			admitted <- err
		}
	}()
	if err := <-admitted; err != nil {
		clear()
		return nil, err
	}
	op := *running
	return &op, nil
}

func (s *Server) modelManagementAPI(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if !p.Authenticated() {
		unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		if !p.Can("models:read") {
			forbidden(w, r)
			return
		}
		storage, models := s.readManagement()
		writeJSON(w, 200, struct {
			Storage   mgmtStorage `json:"storage"`
			Models    []mgmtEntry `json:"models"`
			CanManage bool        `json:"canManage"`
		}{storage, models, p.Can("models:manage")})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, "GET, POST")
		return
	}
	if !p.Can("models:manage") {
		forbidden(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if r.ContentLength > 1024 || len(body) > 1024 {
		w.Header().Set("Connection", "close")
		writeError(w, r, 413, "invalid_request_error", "validation_failed", "Model management body exceeds the 1024 byte limit.", nil)
		return
	}
	var raw any
	if err != nil || json.Unmarshal(body, &raw) != nil {
		badRequest(w, r, "Invalid model management payload.", nil)
		return
	}
	in, ok := raw.(map[string]any)
	modelID, _ := in["modelId"].(string)
	action, _ := in["action"].(string)
	valid := ok && len(in) == 2 && len([]rune(modelID)) >= 1 && utf16Length(modelID) <= 200 &&
		slices.Contains([]string{"install", "uninstall", "enable", "disable", "activate"}, action)
	if !valid {
		badRequest(w, r, "Invalid model management payload.", nil)
		return
	}
	op, err := s.runManagement(modelID, action)
	if err != nil {
		var me *mgmtError
		if !errors.As(err, &me) {
			s.Log.Error("Model management failed.", "event", "models.management-failed", "error", err.Error())
			serviceUnavailable(w, r, "Model management")
			return
		}
		status := 409
		if me.code == "invalid_request" {
			status = 400
		}
		writeError(w, r, status, "invalid_request_error", me.code, me.message, nil)
		return
	}
	status := 200
	if op.State == "running" {
		status = 202
	}
	writeJSON(w, status, op)
}

// WaitManagementIdle waits for an accepted install to settle. Call it after
// the server stopped admitting requests.
func (s *Server) WaitManagementIdle() {
	if s.mgmt != nil {
		s.mgmt.inFlight.Wait()
	}
}
