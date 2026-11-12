package httpx

import (
	"context"
	"net/http"
)

// A change that changes nothing writes no audit line; a refusal is always audited. The audit
// middleware gives each request a note the handler can mark; the handler marks it when its
// change left everything as it was (a disable of what was already off, a save of what was
// already saved).

type unchangedKey struct{}

// WithChangeNote returns r carrying a fresh note, and the note, for the audit middleware.
func WithChangeNote(r *http.Request) (*http.Request, *bool) {
	note := new(bool)
	return r.WithContext(context.WithValue(r.Context(), unchangedKey{}, note)), note
}

// Unchanged marks r's change as having changed nothing. Without a note (a route that isn't
// audited) it does nothing.
func Unchanged(r *http.Request) {
	if note, ok := r.Context().Value(unchangedKey{}).(*bool); ok {
		*note = true
	}
}
