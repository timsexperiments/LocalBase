package obs

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// AuditErr answers for the audit file alone: a warning that a record is missing has to be true.
func TestAuditErrIsAboutTheAuditFile(t *testing.T) {
	write := func(s *Sink) {
		slog.New(NewHandler(s, slog.LevelInfo)).Info("x", "event", "audit.test-changed")
	}
	t.Run("the main log fails and the audit file doesn't", func(t *testing.T) {
		dir := t.TempDir()
		s, err := OpenSink(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_ = s.f.Close() // writes to the main log now fail
		write(s)
		if err := s.AuditErr(); err != nil {
			t.Errorf("the record was written: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl")); err != nil || len(b) == 0 {
			t.Errorf("the audit file: %d bytes, %v", len(b), err)
		}
	})
	t.Run("the audit file can't be opened", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "audit.jsonl"), 0o700); err != nil {
			t.Fatal(err)
		}
		s, err := OpenSink(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		write(s)
		if s.AuditErr() == nil {
			t.Error("the record wasn't written, and nothing says so")
		}
	})
	t.Run("the audit file fails on the write", func(t *testing.T) {
		s, err := OpenSink(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		write(s)
		if err := s.AuditErr(); err != nil {
			t.Fatal(err)
		}
		_ = s.audit.f.Close()
		write(s)
		if s.AuditErr() == nil {
			t.Error("the record wasn't written, and nothing says so")
		}
	})
}

// The audit file's writer goes through rotation, so AuditErr follows each write through that path: a
// failure is reported, a later line that gets in clears it, and a line kept past a failed rotation
// isn't reported as missing.
func TestAuditErrFollowsTheRotatingWrite(t *testing.T) {
	s, err := OpenSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	write := func() { slog.New(NewHandler(s, slog.LevelInfo)).Info("x", "event", "audit.test-changed") }
	write()
	if err := s.AuditErr(); err != nil {
		t.Fatal(err)
	}
	real := s.audit.write
	s.audit.write = func([]byte) (int, error) { return 0, os.ErrPermission }
	write()
	if s.AuditErr() == nil {
		t.Error("a failed write wasn't reported")
	}
	s.audit.write = real
	write()
	if err := s.AuditErr(); err != nil {
		t.Errorf("a line that got in is still reported missing: %v", err)
	}
}
