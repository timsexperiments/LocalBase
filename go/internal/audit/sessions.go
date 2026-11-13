package audit

import (
	"encoding/json"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// UnendedSessions counts sign-outs whose delete failed since everything was last ended:
// each left a session valid until it expires. It reads the event log, because the failure
// was a database write, and counts what the log still holds.
func UnendedSessions(rootDir string) int {
	events, err := obs.Snapshot(rootDir, obs.Filters{}, 5000)
	if err != nil {
		return 0
	}
	last := ""
	for _, ev := range events {
		var head struct {
			EventName string `json:"eventName"`
		}
		if json.Unmarshal(ev.Raw, &head) == nil && head.EventName == "audit.sessions-revoked" && ev.Timestamp > last {
			last = ev.Timestamp
		}
	}
	n := 0
	for _, ev := range events {
		var head struct {
			EventName string `json:"eventName"`
		}
		if json.Unmarshal(ev.Raw, &head) != nil || ev.Timestamp < last {
			continue
		}
		if head.EventName == "auth.session-delete-failed" || head.EventName == "local-session.delete-failed" {
			n++
		}
	}
	return n
}
