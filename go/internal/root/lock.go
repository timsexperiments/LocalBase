package root

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

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
				Host: host, Port: port, InstanceToken: uuid()}
			raw, _ := json.Marshal(owner)
			if err := os.WriteFile(ownerPath, raw, 0o600); err != nil {
				os.RemoveAll(lockDir)
				return nil, err
			}
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
