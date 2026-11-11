package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

// openAuditSink is a variable so that a test can make it fail.
var openAuditSink = obs.OpenSink

// Log is where a command's audit records go: the log of one root. It remembers whether a record
// couldn't be written, for the command's result.
//
// Make a Log with NewAuditLog. A nil *Log is a mistake in the caller, and its methods panic on it,
// as they would on any nil receiver: a change made with no record should never be silent.
type Log struct {
	root *root.Root
	// warnings is where a record that couldn't be written is reported; stderr when it isn't set.
	warnings io.Writer
	// failure is why an audit record of this command wasn't written, as auditFailureClass says it.
	failure string
}

// NewAuditLog is the audit log of r, reporting a record it couldn't write to warnings.
func NewAuditLog(r *root.Root, warnings io.Writer) *Log {
	return &Log{root: r, warnings: warnings}
}

// Record writes one audit event to the root's log, as the gateway does. Failing to log
// does not stop the command, which has already made its change, but it does say so: a
// missing record should not be invisible. Attributes are ids only.
func (a *Log) Record(msg string, attrs ...any) {
	fail := func(err error) {
		a.failure = auditFailureClass(err)
		w := a.warnings
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "Warning: this change was made, but its audit record could not be written: %s (%s)\n", a.failure, cli.Printable(a.root.LogsDir()))
	}
	sink, err := openAuditSink(a.root.LogsDir())
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

// Upgrade records a change of this binary: versions, the new file's short checksum, and how it
// ended. Never a URL, a signature, a path or an error's text: a download URL can carry a token.
func (a *Log) Upgrade(event, from, to, sha256, outcome string) {
	if len(sha256) > 12 {
		sha256 = sha256[:12]
	}
	a.Record("Administration change.", "event", event, "action", strings.TrimPrefix(event, "audit.upgrade-"), "actor_kind", "cli", "actor_id", "",
		"from_version", from, "to_version", to, "build", sha256, "outcome", outcome)
}

// Service records the service being started, stopped or restarted: who, which root, which service,
// and whether it worked. Never the error's text.
func (a *Log) Service(event string, err error, attrs ...any) {
	result, serviceID := "ok", ""
	if err != nil {
		result = "failed"
	}
	if m, merr := service.MetadataFor(a.root.Dir); merr == nil {
		serviceID = m.ServiceID
	}
	a.Record("Administration change.", append([]any{"event", event, "action", strings.TrimPrefix(event, "audit.service-"), "actor_kind", "cli", "actor_id", "",
		"root", a.root.Dir, "service_id", serviceID, "result", result}, attrs...)...)
}

// sayIfInstalled says what's about to happen when it's the installed service, the one on the default
// root, in words, before it happens. Anyone reading a script's output should be able to see it.
func sayIfInstalled(c *cli.Ctx, dir, what string) {
	if root.IsDefault(dir) {
		c.Error("%s", what)
	}
}

// versionForAudit is what a binary said its version is, if it is one: whatever else it says stays out.
func versionForAudit(v string) string {
	if !upgrade.Compare(v, "v0.0.0").Known {
		return "unknown"
	}
	return v
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

// auditTargetRe is what a target can be in the log, as on the routes.
var auditTargetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// Change records a change made from the command line, in the line the route for the
// same change writes: same event, same action word, same target. Unlike a route it isn't
// rate limited, since each line is a process someone on this machine started.
func (a *Log) Change(area, action, targetKind, targetID string, attrs ...any) {
	if !auditTargetRe.MatchString(targetID) {
		targetID = "" // from a file someone else may have written: the same rule as the routes
	}
	a.Record("Administration change.", append([]any{"event", "audit." + area + "-changed", "action", action,
		"actor_kind", "cli", "actor_id", "", "target_kind", targetKind, "target_id", targetID}, attrs...)...)
}

// KeyChange names the key and the permissions it now has, never its secret.
func (a *Log) KeyChange(action string, k *store.APIKey, attrs ...any) {
	a.Change("api-key", action, "api-key", k.ID, append([]any{"scopes", strings.Join(k.Scopes, ",")}, attrs...)...)
}

// AuditOutcome goes in a command's result when the result isn't a map.
type AuditOutcome struct {
	AuditWritten *bool  `json:"auditWritten,omitempty"`
	AuditError   string `json:"auditError,omitempty"`
}

// Outcome is what the result says about this command's records.
func (a *Log) Outcome() AuditOutcome {
	if a.failure == "" {
		return AuditOutcome{}
	}
	written := false
	return AuditOutcome{&written, a.failure}
}

// WithResult says in a --json result that a record of this command couldn't be written. withEnv
// does this for the commands it runs.
func (a *Log) WithResult(data map[string]any) map[string]any {
	if a.failure != "" {
		data["auditWritten"], data["auditError"] = false, a.failure
	}
	return data
}

// PolicyApplied names the policy by its revision: it holds addresses, which don't go in the log.
func (a *Log) PolicyApplied(p *auth.Policy) {
	rev := ""
	if r := auth.Revision(p); r != nil {
		rev = *r
	}
	a.Change("access", "apply-policy", "policy", rev)
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
