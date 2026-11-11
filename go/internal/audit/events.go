package audit

import "strings"

// The events below are the command line's own: each fixes its message, event name and attributes,
// so a line can't drift from the others of its kind. (The sink writes attributes in key order, so
// the order of a call's arguments isn't part of the line.)

// LocalLinkIssued records a local sign-in link: its id (never the token) and whether an unused one
// was replaced.
func (a *Log) LocalLinkIssued(linkID string, forced bool) {
	a.Record("Local sign-in link issued.", "event", "audit.local-login-issued", "link_id", linkID,
		"actor_kind", "cli", "forced", forced)
}

// LocalSessionsRevoked records ending every local session (and any unused link): how many, with no
// session id. LocalSessionRevoked is the same event for one session, named.
func (a *Log) LocalSessionsRevoked(count int) {
	a.Record("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", "cli", "count", count)
}

// LocalSessionRevoked records ending one local session: the same event as LocalSessionsRevoked,
// with its session_id, and a count of 1.
func (a *Log) LocalSessionRevoked(sessionID string) {
	a.Record("Local sessions revoked.", "event", "audit.local-sessions-revoked", "actor_kind", "cli", "session_id", sessionID, "count", 1)
}

// SessionsRevoked records ending every session, of every kind: how many.
func (a *Log) SessionsRevoked(count int) {
	a.Record("All sessions revoked.", "event", "audit.sessions-revoked", "actor_kind", "cli", "count", count)
}

// BackendChanged records a model's backend being set, reset (backend "") or chosen at import: the
// action is "set", "reset" or "import". Ids only: the command a backend runs is left out, since a
// path can hold a person's name.
func (a *Log) BackendChanged(action, modelID, backend string) {
	a.backend("audit.model-backend-changed", action, "model", modelID, backend)
}

// BackendRegistered records a backend definition being added: its id is the target and the backend.
func (a *Log) BackendRegistered(id string) {
	a.backend("audit.backend-registered", "add", "backend", id, id)
}

// BackendRemoved records a backend definition being removed: its id is the target and the backend.
func (a *Log) BackendRemoved(id string) {
	a.backend("audit.backend-removed", "remove", "backend", id, id)
}

// backend is the line the three backend events share.
func (a *Log) backend(event, action, kind, target, backend string) {
	a.Record("Administration change.", "event", event, "action", action, "actor_kind", "cli", "target_kind", kind, "target_id", target, "backend_id", backend)
}

// BackupCreated records a backup being written: whether it holds secrets, and its size.
func (a *Log) BackupCreated(secretsIncluded bool, bytes int64) {
	a.Record("backup.created", "event", "backup.created", "category", "audit", "component", "backup",
		"secretsIncluded", secretsIncluded, "bytes", bytes)
}

// Restored is what a restore did with the root's keys, for BackupRestored. Counts and key ids
// only: never a key's name or secret.
type Restored struct {
	SecretsIncluded bool
	RevocationsKept int
	KeysKept        int
	KeysLeftOut     int
	KeysHeldBack    int
	// KeysBroughtBack is the ids of the keys the restore brought back.
	KeysBroughtBack []string
}

// BackupRestored records a backup being restored into this root.
func (a *Log) BackupRestored(r Restored) {
	a.Record("backup.restored", "event", "backup.restored", "category", "audit", "component", "backup",
		"secretsIncluded", r.SecretsIncluded, "revocationsKept", r.RevocationsKept,
		"keysKept", r.KeysKept, "keysLeftOut", r.KeysLeftOut, "keysHeldBack", r.KeysHeldBack, "keysBroughtBack", strings.Join(r.KeysBroughtBack, ","))
}
