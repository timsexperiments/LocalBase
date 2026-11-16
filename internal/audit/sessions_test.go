package audit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

func TestUnendedSessionCount(t *testing.T) {
	r, err := root.Init(t.TempDir() + "/root")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := obs.OpenSink(r.LogsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	log := slog.New(obs.NewHandler(sink, slog.LevelInfo))
	emit := func(event string) {
		log.Error("x", "event", event)
		time.Sleep(2 * time.Millisecond) // timestamps are in milliseconds
	}
	if n := UnendedSessions(r.Dir); n != 0 {
		t.Errorf("none yet: %d", n)
	}
	emit("auth.session-delete-failed")
	emit("local-session.delete-failed")
	emit("gateway.started")
	if n := UnendedSessions(r.Dir); n != 2 {
		t.Errorf("counted %d", n)
	}
	emit("audit.sessions-revoked")
	if n := UnendedSessions(r.Dir); n != 0 {
		t.Errorf("after ending everything: %d", n)
	}
	emit("auth.session-delete-failed")
	if n := UnendedSessions(r.Dir); n != 1 {
		t.Errorf("a later failure: %d", n)
	}
}
