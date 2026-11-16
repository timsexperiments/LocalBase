package obs

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What someone with authority did is kept apart from everything else that gets logged.
func TestAuditEventsHaveAFileOfTheirOwn(t *testing.T) {
	dir := t.TempDir()
	sink, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(NewHandler(sink, slog.LevelInfo))
	log.Info("A change.", "event", "audit.api-key-changed", "action", "create")
	log.Warn("A request.", "event", "http.request")
	log.Info("Not quite.", "event", "auditing.something")
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	audit, events := auditRead(t, filepath.Join(dir, "audit.jsonl")), auditRead(t, filepath.Join(dir, "events.jsonl"))
	if strings.Count(audit, "\n") != 1 || !strings.Contains(audit, "audit.api-key-changed") {
		t.Errorf("audit.jsonl: %s", audit)
	}
	for _, want := range []string{"audit.api-key-changed", "http.request", "auditing.something"} {
		if !strings.Contains(events, want) {
			t.Errorf("events.jsonl lacks %s", want)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "audit.jsonl"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", info, err)
	}
	// Closing the log closes this file too.
	if _, err := sink.audit.f.Stat(); err == nil {
		t.Error("the audit file is still open")
	}
}

// However much else is written, an audit event stays, where the main log has moved on.
func TestAuditEventsOutlastAFlood(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "logs")
	sink, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	log := slog.New(NewHandler(sink, slog.LevelInfo))
	log.Info("A change.", "event", "audit.api-key-changed", "category", "audit", "target_id", "the-one-that-matters")
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range 70 << 10 { // more than the 60 MiB the main log keeps
		sink.Write(line)
	}
	if got := auditRead(t, filepath.Join(dir, "audit.jsonl")); !strings.Contains(got, "the-one-that-matters") {
		t.Errorf("the audit event is gone: %.200s", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "events*.jsonl"))
	for _, m := range matches {
		if strings.Contains(auditRead(t, m), "the-one-that-matters") {
			t.Errorf("%s still has it, so this flood proved nothing", m)
		}
	}
	events, err := Snapshot(root, Filters{Audit: true}, 10)
	if err != nil || len(events) != 1 || !strings.Contains(string(events[0].Raw), "the-one-that-matters") {
		t.Errorf("%d events, %v", len(events), err)
	}
	// The main log is still what plain reading gets.
	if main, err := Snapshot(root, Filters{}, 10); err != nil || len(main) != 0 {
		t.Errorf("%d events from the main log, %v", len(main), err)
	}
}

// The audit file rotates by itself, keeping as much as the main log does.
func TestAuditFileRotates(t *testing.T) {
	dir := t.TempDir()
	sink, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	line := []byte(strings.Repeat("y", 1023) + "\n")
	for range 90 << 10 { // past what is kept
		sink.writeAudit(line)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "audit*.jsonl"))
	if len(matches) != archives+1 {
		t.Errorf("%d files: %v", len(matches), matches)
	}
	if info, err := os.Stat(filepath.Join(dir, "audit.1.jsonl")); err != nil || info.Size() > rotateBytes {
		t.Errorf("%v %v", info, err)
	}
	if others, _ := filepath.Glob(filepath.Join(dir, "events.*.jsonl")); len(others) != 0 {
		t.Errorf("the main log rotated too: %v", others)
	}
}

// A planted link isn't followed, as for the main log.
func TestAuditFileRefusesALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	sink, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	h := NewHandler(sink, slog.LevelInfo)
	if err := slog.New(h).Handler().Handle(context.Background(), slog.Record{Level: slog.LevelInfo, Message: "x"}); err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("A change.", "event", "audit.api-key-changed")
	if got := auditRead(t, target); got != "" {
		t.Errorf("wrote through the link: %s", got)
	}
}
