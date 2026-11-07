package service

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// --- status ------------------------------------------------------------------------------

// Status mirrors TS ServiceStatus.
type Status struct {
	Manager             string  `json:"manager"`
	ServiceID           string  `json:"serviceId"`
	Root                string  `json:"root"`
	DefinitionPath      string  `json:"definitionPath"`
	DefinitionInstalled bool    `json:"definitionInstalled"`
	Enabled             *bool   `json:"enabled"`
	State               string  `json:"state"`
	ManagerAvailable    bool    `json:"managerAvailable"`
	ManagerState        *string `json:"managerState"`
	PID                 *int    `json:"pid"`
	UptimeSeconds       *int    `json:"uptimeSeconds"`
	RestartCount        *int    `json:"restartCount"`
	BaseURL             string  `json:"baseUrl,omitempty"`
}

// Gateway is the gateway readiness part of an inspection.
type Gateway struct {
	State  string          `json:"state"` // ready | not_ready
	URL    string          `json:"url,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Health json.RawMessage `json:"health,omitempty"`
}

// Inspection is service status plus gateway readiness.
type Inspection struct {
	Service Status  `json:"service"`
	Gateway Gateway `json:"gateway"`
}

// ownerState verifies a live owner by its identity endpoint (#PID reuse).
// VerifiedOwnerState is root.OwnerState with the gateway's identity proven: "active" only when the
// process at the recorded address answers with this root's instance id and root hash. A live PID
// alone can be a reused one, and something else can listen on the port. Anything that sends the
// instance token or writes through the gateway asks this first.
func VerifiedOwnerState(r *root.Root) (state string, o *root.Owner, detail string) {
	return ownerState(r)
}

func ownerState(r *root.Root) (state string, o *root.Owner, detail string) {
	state, o, detail = r.OwnerState()
	if state != "active" {
		return
	}
	url := strings.TrimSuffix(o.HealthURL(), "/health") + "/_localbase/instance"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil { // an address in owner.json that isn't one: not proven
		return "unknown", o, "gateway process is present but did not prove its instance identity"
	}
	req.Header.Set("X-Localbase-Instance-Token", o.InstanceToken)
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "unknown", o, "gateway process is present but did not prove its instance identity"
	}
	defer resp.Body.Close()
	var id struct{ InstanceID, RootHash string }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&id) != nil || id.InstanceID != o.InstanceID || id.RootHash != o.RootHash {
		return "unknown", o, "gateway process is present but did not prove its instance identity"
	}
	return "active", o, ""
}

func readHealth(o *root.Owner) json.RawMessage {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Get(o.HealthURL())
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&raw) != nil {
		return nil
	}
	return raw
}

// Inspect reports the service and gateway state of a root.
func Inspect(r *root.Root, acceptedStart bool) (Inspection, error) {
	m, err := MetadataFor(r.Dir)
	if err != nil {
		return Inspection{}, err
	}
	def, defErr := os.ReadFile(m.DefinitionPath)
	installed := defErr == nil
	obs, err := inspect(m)
	if err != nil {
		obs = observation{state: "unavailable"}
	}
	ownerSt, owner, detail := ownerState(r)
	manifestSt, manifest := readManifest(r.Dir)

	var recorded *root.Owner
	if ownerSt == "active" || ownerSt == "stale" || (ownerSt == "unknown" && owner != nil) {
		recorded = owner
	}
	tokenMatches := recorded != nil && manifest != nil && recorded.ServiceID == m.ServiceID && recorded.ServiceToken == manifest.ServiceToken
	pidMatches := m.Manager != "systemd-user" || (recorded != nil && obs.pid == recorded.PID)
	owned := tokenMatches && pidMatches
	foreground := recorded != nil && recorded.ServiceID == ""
	identityMismatch := recorded != nil && recorded.ServiceID == m.ServiceID && !owned
	manifestIssue := manifestSt == "invalid" || (manifest != nil && (manifest.Root != r.Dir || manifest.RootHash != root.Hash(r.Dir) ||
		manifest.Manager != m.Manager || manifest.ServiceID != m.ServiceID || manifest.DefinitionPath != m.DefinitionPath ||
		(installed && fingerprint(def) != manifest.DefinitionFingerprint))) ||
		(manifestSt == "missing" && (installed || obs.loaded || owned))
	active := ownerSt == "active"

	var state string
	switch {
	case ownerSt == "invalid", identityMismatch:
		state = "unknown"
	case foreground:
		state = map[bool]string{true: "foreground", false: "unknown"}[active]
	case !obs.available, recorded != nil && !owned, manifestIssue:
		state = "unknown"
	case obs.failed, ownerSt == "stale":
		state = "failed"
	case obs.stopping:
		state = "stopping"
	case active && owned && obs.active:
		state = "running"
	case obs.active && (ownerSt == "missing" || (ownerSt == "unknown" && (owned || owner == nil))):
		state = "starting"
	case active && owned && !obs.active:
		state = "unknown"
	case acceptedStart && m.Manager == "launchd" && obs.loaded && !obs.failed &&
		contains([]string{"stopped", "waiting", "spawn scheduled", "xpcproxy", "running"}, obs.state) &&
		(ownerSt == "missing" || (ownerSt == "unknown" && owner == nil)):
		state = "starting"
	case !installed && manifestSt == "missing" && !obs.loaded && ownerSt == "missing":
		state = "not_installed"
	default:
		state = "stopped"
	}
	st := Status{Manager: m.Manager, ServiceID: m.ServiceID, Root: r.Dir, DefinitionPath: m.DefinitionPath,
		DefinitionInstalled: installed, Enabled: obs.enabled, State: state, ManagerAvailable: obs.available}
	if obs.available {
		ms := obs.state
		switch {
		case ownerSt == "invalid":
			ms += "/owner-invalid"
		case identityMismatch:
			ms += "/managed-identity-mismatch"
		case ownerSt == "unknown":
			ms += "/owner-unverified"
		case manifestIssue:
			ms += "/manifest-mismatch"
		}
		st.ManagerState = &ms
	}
	if active {
		pid := owner.PID
		st.PID = &pid
		if t, err := time.Parse(time.RFC3339Nano, owner.StartedAt); err == nil {
			up := max(0, int(time.Since(t).Seconds()))
			st.UptimeSeconds = &up
		}
	}
	if m.Manager == "systemd-user" && obs.hasRestarts {
		n := obs.restarts
		st.RestartCount = &n
	}
	if recorded != nil {
		st.BaseURL = strings.TrimSuffix(recorded.HealthURL(), "/health")
	}
	gw := Gateway{State: "not_ready"}
	switch {
	case active:
		gw.URL = owner.HealthURL()
		if h := readHealth(owner); h != nil && healthOK(h) {
			gw.State, gw.Health = "ready", h
		} else {
			gw.Detail, gw.Health = "gateway health endpoint is unavailable", h
		}
	case ownerSt == "unknown" && owner != nil:
		gw.URL, gw.Detail = owner.HealthURL(), detail
	case ownerSt == "stale":
		gw.URL, gw.Detail = owner.HealthURL(), "gateway owner process is no longer running"
	case ownerSt == "missing":
		gw.Detail = "no active LocalBase instance"
	default:
		gw.Detail = detail
	}
	return Inspection{Service: st, Gateway: gw}, nil
}

func healthOK(raw json.RawMessage) bool {
	var h struct{ Status string }
	return json.Unmarshal(raw, &h) == nil && h.Status == "ok"
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
