package obs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingSink is a sink whose complaints are captured, with the active file nearly full.
func failingSink(t *testing.T) (*Sink, *bytes.Buffer) {
	t.Helper()
	s, err := OpenSink(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var complaints bytes.Buffer
	s.complaints = &complaints
	return s, &complaints
}

// stopRotation makes the active file impossible to move: a read-only directory, as a wrong owner
// or a mount gone read-only would.
func stopRotation(t *testing.T, s *Sink) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	if err := os.Chmod(s.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.dir, 0o700) })
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// The finding: when rotating failed the line was written anyway, so a log that could not rotate
// grew without limit, and nobody was told.
func TestALogThatCannotRotateStaysBoundedAndSaysSo(t *testing.T) {
	s, complaints := failingSink(t)
	nearFull(t, s, 6)
	before := size(t, s.path())
	stopRotation(t, s)
	var mirrored bytes.Buffer
	s.Mirror = &mirrored
	for range 3 {
		s.Write([]byte("over the limit\n"))
	}
	if got := size(t, s.path()); got != before {
		t.Errorf("the log grew from %d to %d bytes while it could not rotate", before, got)
	}
	if n := strings.Count(complaints.String(), "dropping"); n != 1 {
		t.Errorf("want one complaint however many lines are dropped, got: %q", complaints)
	}
	if strings.Count(mirrored.String(), "over the limit") != 3 {
		t.Errorf("foreground output should still show every line: %q", mirrored.String())
	}

	// And it recovers by itself, saying how many were lost.
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("back, and over the limit\n"))
	if got, _ := os.ReadFile(s.path()); string(got) != "back, and over the limit\n" {
		t.Errorf("active after recovering: %d bytes", len(got))
	}
	if !strings.Contains(complaints.String(), "3 events") {
		t.Errorf("recovery should say 3 lines were lost: %q", complaints)
	}
}

// Retrying the rotation must not eat the archives: the chain moved first, so each failed attempt
// pushed the old logs along, and the oldest off the end.
func TestFailedRotationsLeaveTheArchivesAlone(t *testing.T) {
	s, _ := failingSink(t)
	for i := 1; i <= archives; i++ {
		name := filepath.Join(s.dir, "events."+string(rune('0'+i))+".jsonl")
		if err := os.WriteFile(name, []byte{byte('0' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nearFull(t, s, 6)
	// Only the active file is stuck: the name it would move through is a directory with something
	// in it, and the archives could still move.
	stuck := s.rotatingPath()
	if err := os.MkdirAll(filepath.Join(stuck, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	for range archives + 2 {
		s.Write([]byte("over the limit\n"))
	}
	for i := 1; i <= archives; i++ {
		name := filepath.Join(s.dir, "events."+string(rune('0'+i))+".jsonl")
		if got, err := os.ReadFile(name); err != nil || string(got) != string(rune('0'+i)) {
			t.Errorf("archive %d after failed rotations: %q, %v", i, got, err)
		}
	}
}

// A file that cannot be opened again must not move the archives either, and comes back when it can.
func TestALogThatCannotReopenDoesNotLoseArchives(t *testing.T) {
	s, complaints := failingSink(t)
	nearFull(t, s, 6)
	// Force the state a failed open leaves.
	_ = s.f.Close()
	s.f = nil
	if err := os.WriteFile(filepath.Join(s.dir, "events.1.jsonl"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopRotation(t, s)
	if err := os.Chmod(s.path(), 0o000); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("lost\n"))
	s.Write([]byte("lost too\n"))
	if got, _ := os.ReadFile(filepath.Join(s.dir, "events.1.jsonl")); string(got) != "old" {
		t.Errorf("archive 1: %q", got)
	}
	if strings.Count(complaints.String(), "dropping") != 1 {
		t.Errorf("complaints: %q", complaints)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path(), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("back\n"))
	if got, _ := os.ReadFile(s.path()); !strings.HasSuffix(string(got), "back\n") {
		t.Errorf("after reopening: %d bytes", len(got))
	}
}

// A short write (a full disk) is reported and the size stays true. What it left is a torn line, so
// the next line starts on its own: one torn line costs one event, not two.
func TestAShortWriteIsReportedAndDoesNotTearTheNextLine(t *testing.T) {
	s, complaints := failingSink(t)
	real := s.write
	s.write = func(p []byte) (int, error) {
		n, _ := real(p[:len(p)/2])
		return n, errors.New("no space left on device")
	}
	s.Write([]byte(`{"first":1}` + "\n"))
	if !strings.Contains(complaints.String(), "no space left") {
		t.Errorf("complaints: %q", complaints)
	}
	if got := size(t, s.path()); got != s.size {
		t.Errorf("size says %d, the file has %d", s.size, got)
	}
	s.write = real
	s.Write([]byte(`{"second":2}` + "\n"))
	got, _ := os.ReadFile(s.path())
	lines := strings.Split(string(got), "\n")
	if len(lines) != 3 || lines[1] != `{"second":2}` {
		t.Errorf("the next line was not whole: %q", got)
	}
}

// Archives that can't move cost the old logs, not the new line, and it is reported.
func TestArchivesThatCannotMoveAreReportedAndTheLineIsKept(t *testing.T) {
	s, complaints := failingSink(t)
	for i := 1; i <= archives; i++ {
		name := filepath.Join(s.dir, "events."+string(rune('0'+i))+".jsonl")
		if err := os.MkdirAll(filepath.Join(name, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nearFull(t, s, 6)
	s.Write([]byte("over the limit\n"))
	if got, _ := os.ReadFile(s.path()); string(got) != "over the limit\n" {
		t.Errorf("active: %d bytes", len(got))
	}
	if !strings.Contains(complaints.String(), "could not keep the last events log") {
		t.Errorf("complaints: %q", complaints)
	}
	if strings.Contains(complaints.String(), s.dir) {
		t.Errorf("a complaint names the path: %q", complaints)
	}
	if strings.Contains(complaints.String(), "dropping") {
		t.Errorf("nothing was dropped: %q", complaints)
	}
}

// Writing after Close is the end of shutdown, not a failing log.
func TestWritingAfterCloseIsQuiet(t *testing.T) {
	s, complaints := failingSink(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var mirrored bytes.Buffer
	s.Mirror = &mirrored
	s.Write([]byte("late\n"))
	if complaints.Len() != 0 {
		t.Errorf("complaints: %q", complaints)
	}
	if mirrored.String() != "late\n" {
		t.Errorf("mirror: %q", mirrored.String())
	}
}
