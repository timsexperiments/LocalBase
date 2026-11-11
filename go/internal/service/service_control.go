package service

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/localhttp"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// --- control ---------------------------------------------------------------------------

func assertControllable(r *root.Root, m Metadata, token, op string) error {
	state, o, detail := ownerState(r)
	switch {
	case state == "invalid", state == "unknown" && o == nil:
		return errorf("Cannot %s LocalBase for %s: %s.", op, r.Dir, detail)
	case (state == "active" || state == "unknown") && o.ServiceID == m.ServiceID && o.ServiceToken != token:
		return errorf("Cannot %s LocalBase for %s: the managed gateway identity does not match this service definition.", op, r.Dir)
	case (state == "active" || state == "unknown") && o.ServiceID != m.ServiceID:
		who := "a foreground gateway"
		if o.ServiceID != "" {
			who = "another managed gateway (" + o.ServiceID + ")"
		}
		return errorf("Cannot %s LocalBase for %s: %s owns the root. Stop it explicitly before managing the user service.", op, r.Dir, who)
	}
	return nil
}

func enable(m Metadata) error {
	if m.Manager == "launchd" {
		return must(m, "enable", target(m))
	}
	return must(m, "--user", "enable", m.UnitName)
}

func disable(m Metadata) error {
	if m.Manager == "launchd" {
		return must(m, "disable", target(m))
	}
	res, err := run(m, "--user", "disable", m.UnitName)
	if err != nil || res.code == 0 {
		return err
	}
	low := strings.ToLower(res.stdout + "\n" + res.stderr)
	if strings.Contains(low, "does not exist") || strings.Contains(low, "not found") {
		return nil
	}
	return must(m, "--user", "disable", m.UnitName)
}

func startManager(d Definition) error {
	if d.Manager == "launchd" {
		if err := enable(d.Metadata); err != nil {
			return err
		}
		return must(d.Metadata, "bootstrap", domain(), d.DefinitionPath)
	}
	if err := must(d.Metadata, "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := enable(d.Metadata); err != nil {
		return err
	}
	return must(d.Metadata, "--user", "start", d.UnitName)
}

func stopManager(m Metadata, alsoDisable bool) error {
	before, err := inspect(m)
	if err != nil {
		return err
	}
	if m.Manager == "launchd" {
		if before.loaded {
			if err := must(m, "bootout", target(m)); err != nil {
				return err
			}
		}
	} else if before.loaded || before.active {
		if err := must(m, "--user", "stop", m.UnitName); err != nil {
			return err
		}
	}
	if alsoDisable {
		if err := disable(m); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(managerTimeout)
	for time.Now().Before(deadline) {
		if o, err := inspect(m); err == nil && o.stopped(m) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errorf("%s did not stop %s within %dms.", m.Manager, m.ServiceID, managerTimeout.Milliseconds())
}

func waitOwnerGone(r *root.Root) error {
	deadline := time.Now().Add(managerTimeout)
	for time.Now().Before(deadline) {
		state, _, detail := r.OwnerState()
		switch state {
		case "missing":
			return nil
		case "stale":
			if r.ClearStale() == nil {
				return nil
			}
		case "invalid":
			return errorf("Gateway ownership became invalid while stopping %s: %s.", r.Dir, detail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errorf("LocalBase gateway for %s did not release ownership within %dms. No process was signaled because its identity could not be proven.",
		r.Dir, managerTimeout.Milliseconds())
}

func stopManaged(r *root.Root, m Metadata, alsoDisable bool) error {
	token := ""
	if st, man := readManifest(r.Dir); st == "valid" && man.Root == r.Dir && man.ServiceID == m.ServiceID {
		token = man.ServiceToken
	}
	if err := assertControllable(r, m, token, "stop"); err != nil {
		return err
	}
	r.ClearStale()
	state, o, _ := ownerState(r)
	obs, err := inspect(m)
	if err != nil {
		return err
	}
	if (state == "active" || state == "unknown") && o != nil && o.ServiceID == m.ServiceID && !obs.active && !obs.stopping {
		return errorf("Cannot stop LocalBase for %s: the recorded managed gateway is not controlled by the current %s instance. No process was signaled.", r.Dir, m.Manager)
	}
	if err := stopManager(m, alsoDisable); err != nil {
		return err
	}
	return waitOwnerGone(r)
}

// Start installs and starts the service (restart replaces a running one
// with a fresh token).
// Start installs and starts the service, holding the root operation lease and
// handing it to the gateway it launches.
func Start(r *root.Root, restart, installMissing bool) (Inspection, error) {
	lease, err := lease.AcquireOperation(r.Dir, "service-start")
	if err != nil {
		return Inspection{}, err
	}
	defer lease.Release()
	return start(r, lease, restart, installMissing)
}

// RestartWithin restarts the service for a caller holding the operation
// lease (TS restartServiceWithinOperation); the lease is handed to the new
// gateway.
func RestartWithin(r *root.Root, lease *lease.Lease) (Inspection, error) {
	return start(r, lease, true, false)
}

func start(r *root.Root, lease *lease.Lease, restart, installMissing bool) (Inspection, error) {
	if err := refuseDefaultRootInTest(r.Dir); err != nil {
		return Inspection{}, err
	}
	st, prev := readManifest(r.Dir)
	current := ""
	if st == "valid" && prev.Root == r.Dir {
		current = prev.ServiceToken
	}
	token := current
	if restart || token == "" {
		token = store.UUID()
	}
	inv, err := ResolveInvocation(r.Dir, installMissing)
	if err != nil {
		return Inspection{}, err
	}
	d, err := newDefinition(r.Dir, inv, token)
	if err != nil {
		return Inspection{}, err
	}
	check := current
	if check == "" {
		check = token
	}
	op := "start"
	if restart {
		op = "restart"
	}
	if err := assertControllable(r, d.Metadata, check, op); err != nil {
		return Inspection{}, err
	}
	r.ClearStale()
	state, o, _ := ownerState(r)
	obs, err := inspect(d.Metadata)
	if err != nil {
		return Inspection{}, err
	}
	installedDef, _ := os.ReadFile(d.DefinitionPath)
	unchanged := st == "valid" && prev.DefinitionFingerprint == d.Manifest.DefinitionFingerprint &&
		prev.ServiceID == d.ServiceID && prev.Root == r.Dir && fingerprint(installedDef) == d.Manifest.DefinitionFingerprint
	managedOwner := o != nil && o.ServiceID == d.ServiceID && (state == "active" || state == "unknown")
	if !restart && unchanged && obs.active && (managedOwner || state == "missing") {
		if err := enable(d.Metadata); err != nil {
			return Inspection{}, err
		}
		return Inspect(r, false)
	}
	if obs.loaded || obs.active || obs.stopping || managedOwner {
		if err := stopManaged(r, d.Metadata, true); err != nil {
			return Inspection{}, err
		}
	}
	if err := d.write(r.Dir); err != nil {
		return Inspection{}, err
	}
	if err := startManager(d); err != nil {
		return Inspection{}, err
	}
	if err := lease.Handoff(token); err != nil {
		return Inspection{}, err
	}
	return Inspect(r, true)
}

// refuseDefaultRootInTest keeps a test binary away from the installed service: a test that forgot its
// root would otherwise stop the real gateway on this machine.
func refuseDefaultRootInTest(dir string) error {
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") && root.IsDefault(dir) {
		return errorf("A test tried to manage the service on the default root %s. Give it a scratch root.", dir)
	}
	return nil
}

// Stop stops and disables the service under the root operation lease.
func Stop(r *root.Root) (in Inspection, err error) {
	err = lease.WithOperation(r.Dir, "stop", func() error {
		in, err = StopWithin(r)
		return err
	})
	return in, err
}

// StopWithin is Stop for a caller that holds the operation lease.
func StopWithin(r *root.Root) (Inspection, error) {
	if err := refuseDefaultRootInTest(r.Dir); err != nil {
		return Inspection{}, err
	}
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	if err := stopManaged(r, m, true); err != nil {
		return Inspection{}, err
	}
	return Inspect(r, false)
}

// HasEvidence reports whether anything of a service exists for the root.
func HasEvidence(r *root.Root) bool {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return false
	}
	_, defErr := os.Stat(m.DefinitionPath)
	_, manErr := os.Stat(manifestPath(r.Dir))
	state, _, _ := r.OwnerState()
	return defErr == nil || manErr == nil || state != "missing"
}

// Remove stops the service and deletes its definition and manifest. The
// caller holds the operation lease.
func Remove(r *root.Root) (Inspection, error) {
	if err := refuseDefaultRootInTest(r.Dir); err != nil {
		return Inspection{}, err
	}
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	if err := stopManaged(r, m, true); err != nil {
		return Inspection{}, err
	}
	// A definition left behind is loaded again at the next login, so it is never left silently.
	if err := removeIfThere(m.DefinitionPath); err != nil {
		return Inspection{}, errorf("Stopped the LocalBase service, but could not remove its definition, %s: %v. Remove that file by hand, or the service may start again at the next login.", m.DefinitionPath, err)
	}
	manifestErr := removeIfThere(manifestPath(r.Dir))
	if m.Manager == "systemd-user" {
		if err := must(m, "--user", "daemon-reload"); err != nil {
			return Inspection{}, err
		}
	}
	if manifestErr != nil {
		return Inspection{}, errorf("Stopped the LocalBase service, but could not remove %s: %v. Remove that file by hand, or LocalBase will still report a service here.", manifestPath(r.Dir), manifestErr)
	}
	return Inspect(r, false)
}

// removeIfThere removes path, and is fine if it's already gone. A failure gives its cause alone,
// since the message it goes in names the path.
func removeIfThere(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// WaitReady waits until the gateway answers /health/ready with ready and
// no restart is pending.
func WaitReady(r *root.Root, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if in, err := Inspect(r, false); err == nil && in.Gateway.State == "ready" && answersReady(in.Gateway.URL) && !restartPending(r) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("Timed out waiting for LocalBase gateway readiness.")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// answersReady asks the gateway whose health URL is healthURL for /health/ready, on the local
// client (no proxy, no redirect).
func answersReady(healthURL string) bool {
	resp, err := localhttp.New(750 * time.Millisecond).Get(strings.TrimSuffix(healthURL, "/health") + "/health/ready")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct{ Status string }
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode == 200 && body.Status == "ready"
}

// restartPending reports a recorded pending restart (read-only).
func restartPending(r *root.Root) bool {
	st, err := store.OpenReadOnly(r.DB())
	if err != nil {
		return false
	}
	defer st.Close()
	pending, _ := st.RestartPending(context.Background())
	return pending
}
