package root

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// gatewayOwner is the owner record the TS gateway writes inside its lock
// directory. Writing the same shape keeps the two implementations mutually
// exclusive on one root and lets either CLI report who owns it.
type gatewayOwner struct {
	Version       int    `json:"version"`
	InstanceID    string `json:"instanceId"`
	Root          string `json:"root"`
	RootHash      string `json:"rootHash"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	InstanceToken string `json:"instanceToken"`
	ServiceID     string `json:"serviceId,omitempty"`
	ServiceToken  string `json:"serviceToken,omitempty"`
}

// Owner is the recorded gateway owner of a root.
type Owner = gatewayOwner

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Lock takes the gateway lock for the life of the process: the directory
// `runtime/gateway.lock` with an `owner.json`, as the TS gateway does. A lock
// left by a dead process is removed.
func (r *Root) Lock(host string, port int) (release func(), err error) {
	return r.LockAs(host, port, "", "")
}

// LockAs is Lock for a managed service, recording its id and token (both or
// neither) so `status` can tell the service from a foreground gateway.
func (r *Root) LockAs(host string, port int, serviceID, serviceToken string) (release func(), err error) {
	if (serviceID == "") != (serviceToken == "") {
		serviceID, serviceToken = "", ""
	}
	runtimeDir := filepath.Join(r.Dir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(runtimeDir, "gateway.lock")
	ownerPath := filepath.Join(lockDir, "owner.json")
	for attempt := 0; attempt < 2; attempt++ {
		if err := os.Mkdir(lockDir, 0o700); err == nil {
			owner := gatewayOwner{Version: 2, InstanceID: uuid(), Root: r.Dir, RootHash: Hash(r.Dir),
				PID: os.Getpid(), StartedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				Host: host, Port: port, InstanceToken: uuid(), ServiceID: serviceID, ServiceToken: serviceToken}
			raw, _ := json.Marshal(owner)
			if err := os.WriteFile(ownerPath, raw, 0o600); err != nil {
				os.RemoveAll(lockDir)
				return nil, err
			}
			r.owner = &owner
			return func() { os.RemoveAll(lockDir) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		var owner gatewayOwner
		raw, readErr := os.ReadFile(ownerPath)
		if readErr == nil {
			readErr = json.Unmarshal(raw, &owner)
		}
		if readErr == nil && processAlive(owner.PID) {
			return nil, fmt.Errorf("another LocalBase gateway (pid %d) is serving %s", owner.PID, r.Dir)
		}
		if readErr != nil {
			// A lock directory without a readable owner may be mid-creation.
			if info, err := os.Stat(lockDir); err == nil && time.Since(info.ModTime()) < 10*time.Second {
				return nil, fmt.Errorf("another LocalBase gateway is starting on %s", r.Dir)
			}
		}
		if err := os.RemoveAll(lockDir); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not take the gateway lock on %s", r.Dir)
}

// OwnerState is what the lock directory says about the gateway owner:
// missing, active (process alive), stale (process gone) or invalid.
func (r *Root) OwnerState() (state string, owner *Owner, detail string) {
	raw, err := os.ReadFile(filepath.Join(r.Dir, "runtime", "gateway.lock", "owner.json"))
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(r.Dir, "runtime", "gateway.lock")); statErr == nil {
			return "unknown", nil, "gateway ownership is being initialized"
		}
		return "missing", nil, ""
	}
	if err != nil {
		return "invalid", nil, err.Error()
	}
	var o Owner
	if err := json.Unmarshal(raw, &o); err != nil || o.Version != 2 || o.PID <= 0 || o.Root != r.Dir {
		return "invalid", nil, "gateway owner record is invalid"
	}
	if !processAlive(o.PID) {
		return "stale", &o, ""
	}
	return "active", &o, ""
}

// ClearStale removes a lock left by a dead owner.
func (r *Root) ClearStale() error {
	if state, _, _ := r.OwnerState(); state == "stale" {
		return os.RemoveAll(filepath.Join(r.Dir, "runtime", "gateway.lock"))
	}
	return nil
}

// Instance is the owner record this process wrote when it took the lock.
func (r *Root) Instance() *Owner { return r.owner }

// HealthURL is the owner's /health URL (wildcard hosts mapped to loopback).
func (o *Owner) HealthURL() string {
	h := o.Host
	switch {
	case h == "0.0.0.0":
		h = "127.0.0.1"
	case h == "::" || h == "[::]":
		h = "[::1]"
	case strings.Contains(h, ":") && !strings.HasPrefix(h, "["):
		h = "[" + h + "]"
	}
	return fmt.Sprintf("http://%s:%d/health", h, o.Port)
}
