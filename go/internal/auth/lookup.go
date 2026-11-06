package auth

import (
	"context"
	"sync/atomic"
)

type lookupKey struct{}

// WithLookupState gives a request somewhere to say that looking up who it is failed. A failed lookup
// says nothing about the credential, and whatever answers the request has to know that.
func WithLookupState(ctx context.Context) context.Context {
	return context.WithValue(ctx, lookupKey{}, &atomic.Bool{})
}

// MarkLookupFailed records that a key, session or policy couldn't be read for this request.
func MarkLookupFailed(ctx context.Context) {
	if failed, ok := ctx.Value(lookupKey{}).(*atomic.Bool); ok {
		failed.Store(true)
	}
}

// LookupFailed reports whether MarkLookupFailed was called for this request.
func LookupFailed(ctx context.Context) bool {
	failed, ok := ctx.Value(lookupKey{}).(*atomic.Bool)
	return ok && failed.Load()
}
