package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

// openAuditSink is a variable so that a test can make it fail.
var openAuditSink = obs.OpenSink

// auditCLI writes one audit event to the root's log, as the gateway does. Failing to log
// does not stop the command, which has already made its change, but it does say so: a
// missing record should not be invisible. Attributes are ids only.
func auditCLI(e *env, msg string, attrs ...any) {
	fail := func(err error) {
		e.auditFailure = auditFailureClass(err)
		w := e.warnings
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "Warning: this change was made, but its audit record could not be written: %s (%s)\n", e.auditFailure, e.Root.LogsDir())
	}
	sink, err := openAuditSink(e.Root.LogsDir())
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = sink.Close() }() // the event is written; a failed close changes nothing
	r := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
	r.Add(attrs...)
	if err := obs.NewHandler(sink, slog.LevelInfo).Handle(context.Background(), r); err != nil {
		fail(err)
		return
	}
	if err := sink.AuditErr(); err != nil {
		fail(err)
	}
}

// auditUpgrade records a change of this binary: versions, the new file's short checksum, and how it
// ended. Never a URL, a signature, a path or an error's text: a download URL can carry a token.
func auditUpgrade(e *env, event, from, to, sha256, outcome string) {
	if len(sha256) > 12 {
		sha256 = sha256[:12]
	}
	auditCLI(e, "Administration change.", "event", event, "action", strings.TrimPrefix(event, "audit.upgrade-"), "actor_kind", "cli", "actor_id", "",
		"from_version", from, "to_version", to, "build", sha256, "outcome", outcome)
}

// upgradeOutcome is how an Apply ended, in words from a short list.
func upgradeOutcome(res upgrade.Result, err error) string {
	var smoke *upgrade.SmokeFailed
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

// auditFailureClass names what went wrong from a short list. An error's own text is never
// shown, as it can hold anything: the warning goes to a terminal and to whatever captures it.
func auditFailureClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission-denied"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "disk-full"
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR):
		return "not-a-file"
	}
	return "write-failed"
}

// auditChange records a change made from the command line, in the line the route for the
// same change writes: same event, same action word, same target. Unlike a route it isn't
// rate limited, since each line is a process someone on this machine started.
func auditChange(e *env, area, action, targetKind, targetID string, attrs ...any) {
	auditCLI(e, "Administration change.", append([]any{"event", "audit." + area + "-changed", "action", action,
		"actor_kind", "cli", "actor_id", "", "target_kind", targetKind, "target_id", targetID}, attrs...)...)
}

// auditKeyChange names the key and the permissions it now has, never its secret.
func auditKeyChange(e *env, action string, k *store.APIKey, attrs ...any) {
	auditChange(e, "api-key", action, "api-key", k.ID, append([]any{"scopes", strings.Join(k.Scopes, ",")}, attrs...)...)
}

// auditOutcome goes in a command's result when the result isn't a map.
type auditOutcome struct {
	AuditWritten *bool  `json:"auditWritten,omitempty"`
	AuditError   string `json:"auditError,omitempty"`
}

func (e *env) auditOutcome() auditOutcome {
	if e.auditFailure == "" {
		return auditOutcome{}
	}
	written := false
	return auditOutcome{&written, e.auditFailure}
}

// withAuditResult says in a --json result that a record of this command couldn't be written. withEnv
// does this for the commands it runs.
func (e *env) withAuditResult(data map[string]any) map[string]any {
	if e.auditFailure != "" {
		data["auditWritten"], data["auditError"] = false, e.auditFailure
	}
	return data
}

// auditPolicyApplied names the policy by its revision: it holds addresses, which don't go in the log.
func auditPolicyApplied(e *env, p *auth.Policy) {
	rev := ""
	if r := auth.Revision(p); r != nil {
		rev = *r
	}
	auditChange(e, "access", "apply-policy", "policy", rev)
}

// userAction is the route's word for what userCommand does, by the status it sets.
func userAction(status string) string {
	switch status {
	case "":
		return "remove-user"
	case "active":
		return "enable-user"
	}
	return "disable-user"
}
