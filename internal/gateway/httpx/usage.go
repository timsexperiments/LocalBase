package httpx

import (
	"net/http"
	"time"
)

// UsageNote is what a handler knows that the middleware cannot see.
type UsageNote struct {
	Model     string
	Trusted   bool // the model came from a lease, so the gateway resolved it
	Outcome   string
	Status    int
	In, Out   int64
	HasTokens bool
	Queue     time.Duration
}

func NoteFrom(r *http.Request) *UsageNote {
	if rec, ok := r.Context().Value(RecKey{}).(*Recorder); ok {
		return &rec.Usage
	}
	return nil
}
