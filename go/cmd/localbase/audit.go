package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// auditCLI writes one audit event to the root's log, as the gateway does. Failing to log
// does not stop the command, which has already made its change, but it does say so: a
// missing record should not be invisible. Attributes are ids only.
func auditCLI(e *env, msg string, attrs ...any) {
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "Warning: this change was made, but its audit record could not be written: %v\n", err)
	}
	sink, err := obs.OpenSink(e.Root.LogsDir())
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = sink.Close() }() // the event is written; a failed close changes nothing
	r := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
	r.Add(attrs...)
	if err := obs.NewHandler(sink, slog.LevelInfo).Handle(context.Background(), r); err != nil {
		fail(err)
	}
}

// auditChange records a change made from the command line, in the line the route for the
// same change writes: same event, same action word, same target. Unlike a route it isn't
// rate limited, since each line is a process someone on this machine started.
func auditChange(e *env, area, action, targetKind, targetID string, attrs ...any) {
	auditCLI(e, "Administration change.", append([]any{"event", "audit." + area + "-changed", "action", action,
		"actor_kind", "cli", "actor_id", "", "target_kind", targetKind, "target_id", targetID}, attrs...)...)
}

// auditKeyChange names the key and the permissions it now has, never its secret.
func auditKeyChange(e *env, action string, k *store.APIKey) {
	auditChange(e, "api-key", action, "api-key", k.ID, "scopes", strings.Join(k.Scopes, ","))
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
