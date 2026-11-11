package upgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/localhttp"
)

// The decisions an upgrade makes, apart from the binary steps (Prepare, Apply, Rollback): whether
// the gateway running on a root can be upgraded under, whether it is busy, and how the upgrade
// ended, in words. The command line composes these with the service, store and audit log.

// GatewayState is what runs on this root, for an upgrade's purposes.
type GatewayState int

const (
	GatewayNone GatewayState = iota
	GatewayManaged
)

// ClassifyGateway decides from the facts, so each refusal can be tested on its own.
type GatewayFacts struct {
	LockState, LockDetail          string
	OwnerManaged                   bool
	DefinitionInstalled, Reachable bool
	ServiceProgram, Binary         string
}

func ClassifyGateway(f GatewayFacts) (GatewayState, error) {
	switch f.LockState {
	case "missing", "stale":
		return GatewayNone, nil
	case "active":
	default:
		detail := f.LockDetail
		if detail == "" {
			detail = "its lock could not be read"
		}
		return 0, errors.New("Cannot tell whether a gateway is running on this root: " + detail + ". Nothing was changed.")
	}
	if !f.OwnerManaged {
		return 0, errors.New("A gateway is running on this root in the foreground. Stop it first: this cannot restart what it did not start. Nothing was changed.")
	}
	if !f.DefinitionInstalled || !f.Reachable {
		return 0, errors.New("A gateway is running under the service manager, but the service manager cannot be reached, so it could not be restarted safely. Nothing was changed.")
	}
	if f.ServiceProgram != f.Binary {
		return 0, fmt.Errorf("The service on this root runs %s, not this binary (%s), so this could not restart it safely. Run that one's upgrade. Nothing was changed.", f.ServiceProgram, f.Binary)
	}
	return GatewayManaged, nil
}

// ParseActivity reads the gateway's answer to /_localbase/host: whether it is busy, and what with,
// in words. It reads at most localhttp.MaxAnswer of the body. Anything but a 200 whose body fits
// and decodes is "the gateway answered HTTP <status>".
func ParseActivity(status int, body io.Reader) (busy bool, detail string, err error) {
	var answer struct {
		Host struct {
			Activity struct {
				Busy bool `json:"busy"`
				Runs struct {
					Active int `json:"active"`
				} `json:"runs"`
				LLM, STT, TTS, Image, Video struct {
					InFlight int `json:"inFlight"`
					Queued   int `json:"queued"`
				}
			} `json:"activity"`
		} `json:"host"`
	}
	b, err := io.ReadAll(io.LimitReader(body, localhttp.MaxAnswer+1))
	if status != http.StatusOK || err != nil || len(b) > localhttp.MaxAnswer || json.NewDecoder(bytes.NewReader(b)).Decode(&answer) != nil {
		return false, "", fmt.Errorf("the gateway answered HTTP %d", status)
	}
	a := answer.Host.Activity
	work := a.LLM.InFlight + a.STT.InFlight + a.TTS.InFlight + a.Image.InFlight + a.Video.InFlight
	queued := a.LLM.Queued + a.STT.Queued + a.TTS.Queued + a.Image.Queued + a.Video.Queued
	return a.Busy, fmt.Sprintf("%d in flight, %d queued, %d detached runs", work, queued, a.Runs.Active), nil
}

// WaitIdle waits for a gateway's work to finish, and cuts nothing if it does not.
func WaitIdle(ask func() (bool, string, error), limit time.Duration, sleep func(time.Duration)) error {
	deadline := time.Now().Add(limit)
	for {
		busy, detail, err := ask()
		if err != nil {
			return fmt.Errorf("Could not ask the gateway how busy it is: %w. Nothing was changed. --force goes ahead anyway.", err)
		}
		if !busy {
			return nil
		}
		if limit <= 0 {
			return fmt.Errorf("The gateway is busy: %s. Nothing was changed. --wait <seconds> waits for it to finish, and --force goes ahead, cutting that work.", detail)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("The gateway is still busy after waiting: %s. Nothing was changed, and nothing was cut. --force goes ahead, cutting that work.", detail)
		}
		sleep(time.Second)
	}
}

// VersionForAudit is what a binary said its version is, if it is one: whatever else it says stays out.
func VersionForAudit(v string) string {
	if !Compare(v, "v0.0.0").Known {
		return "unknown"
	}
	return v
}

// ApplyOutcome is how an Apply ended, in words from a short list.
func ApplyOutcome(res Result, err error) string {
	var smoke *SmokeFailed
	switch {
	case err == nil:
		return "applied"
	case res.RolledBack:
		return "rolled-back"
	case errors.As(err, &smoke):
		return "smoke-failed"
	}
	return "failed"
}
