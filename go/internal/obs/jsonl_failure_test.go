package obs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
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

// stuckAudit is a sink with its audit log started, its limit nearly reached and rotation impossible.
func stuckAudit(t *testing.T) (*Sink, *Sink, *bytes.Buffer) {
	t.Helper()
	s, complaints := failingSink(t)
	s.writeAudit([]byte("first\n"))
	a := s.audit
	if err := os.Truncate(a.path(), rotateBytes-6); err != nil {
		t.Fatal(err)
	}
	a.size = rotateBytes - 6
	stopRotation(t, s)
	return s, a, complaints
}

// The main log drops lines in an outage. An audit log keeps them, in a small overrun.
func TestAnAuditLogThatCannotRotateKeepsItsEvents(t *testing.T) {
	s, a, complaints := stuckAudit(t)
	for range 3 {
		s.writeAudit([]byte("what someone did\n"))
	}
	got, _ := os.ReadFile(a.path())
	if n := strings.Count(string(got), "what someone did"); n != 3 {
		t.Errorf("%d of 3 audit events in the file", n)
	}
	if strings.Count(complaints.String(), "past its limit") != 1 {
		t.Errorf("complaints: %q", complaints)
	}
	out := s.Outages()
	if len(out) != 1 || out[0].Log != "audit" || out[0].Path != a.path() || out[0].Dropped != 0 ||
		out[0].SizeBytes != a.size || !strings.Contains(out[0].Reason, "permission denied") || out[0].Since == "" {
		t.Errorf("outages: %+v", out)
	}
	if strings.Contains(out[0].Reason, s.dir) {
		t.Errorf("the reason repeats the path: %q", out[0].Reason)
	}

	// And it catches up by itself.
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.writeAudit([]byte("back\n"))
	if len(s.Outages()) != 0 {
		t.Errorf("still reported: %+v", s.Outages())
	}
	if got, _ := os.ReadFile(a.path()); string(got) != "back\n" {
		t.Errorf("did not rotate on recovering: %d bytes", len(got))
	}
}

// The overrun is small and it ends: past it the file stops growing and events go to stderr, whole,
// and past that they are lost, and it says so once. The file and stderr both stay bounded.
func TestAnAuditLogPastItsOverrunGoesToStderrThenSaysItIsLosing(t *testing.T) {
	s, a, complaints := stuckAudit(t)
	if err := os.Truncate(a.path(), rotateBytes+auditOverrun); err != nil {
		t.Fatal(err)
	}
	a.size = rotateBytes + auditOverrun
	before := a.size
	s.writeAudit([]byte("keep this\n"))
	s.writeAudit([]byte("and this\n"))
	if size(t, a.path()) != before {
		t.Errorf("the file grew past its overrun: %d", size(t, a.path()))
	}
	if !strings.Contains(complaints.String(), "keep this\n") || !strings.Contains(complaints.String(), "and this\n") {
		t.Errorf("complaints: %q", complaints)
	}
	if strings.Count(complaints.String(), "its events follow") != 1 {
		t.Errorf("complaints: %q", complaints)
	}
	if out := s.Outages(); len(out) != 1 || out[0].OnStderr != 2 || out[0].Dropped != 0 {
		t.Errorf("outages: %+v", out)
	}

	a.spilled = auditOverrun - 4
	written := complaints.Len()
	s.writeAudit([]byte("over\n"))
	s.writeAudit([]byte("over\n"))
	if strings.Contains(complaints.String()[written:], "over\n") {
		t.Errorf("stderr is not bounded: %q", complaints.String()[written:])
	}
	if strings.Count(complaints.String(), "LOST") != 1 {
		t.Errorf("complaints: %q", complaints)
	}
	if out := s.Outages(); len(out) != 1 || out[0].Dropped != 2 {
		t.Errorf("outages: %+v", out)
	}
}

// A file that can't even be opened used to lose the audit line in silence.
func TestAnAuditLogThatCannotOpenDoesNotLoseTheLineSilently(t *testing.T) {
	s, complaints := failingSink(t)
	if err := os.Mkdir(filepath.Join(s.dir, "audit.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.writeAudit([]byte("what someone did\n"))
	if !strings.Contains(complaints.String(), "what someone did\n") {
		t.Errorf("complaints: %q", complaints)
	}
	if out := s.Outages(); len(out) != 1 || out[0].Log != "audit" || out[0].OnStderr != 1 {
		t.Errorf("outages: %+v", out)
	}
}

// The main log's outage is reported too, with what it lost, and ends when the log recovers.
func TestOutagesReportTheMainLog(t *testing.T) {
	s, _ := failingSink(t)
	if out := s.Outages(); len(out) != 0 {
		t.Errorf("a healthy log: %+v", out)
	}
	nearFull(t, s, 6)
	stopRotation(t, s)
	s.Write([]byte("over the limit\n"))
	s.Write([]byte("over the limit\n"))
	out := s.Outages()
	if len(out) != 1 || out[0].Log != "events" || out[0].Dropped != 2 || out[0].Path != s.path() || out[0].SizeBytes != s.size {
		t.Fatalf("outages: %+v", out)
	}
	since, err := time.Parse(time.RFC3339, out[0].Since)
	if err != nil {
		t.Fatal(err)
	}
	line := out[0].Describe(since.Add(90 * time.Second))
	for _, want := range []string{"events log", s.path(), "permission denied", "1m30s ago", "2 events lost"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %q", line, want)
		}
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("over the limit\n"))
	if out := s.Outages(); len(out) != 0 {
		t.Errorf("after recovering: %+v", out)
	}
}

// The command line writes its audit events to the same file as a running gateway, so what the
// gateway counted is not the whole file.
func TestAnotherProcessAppendingCountsTowardsTheLimit(t *testing.T) {
	s, _ := failingSink(t)
	other, err := OpenSink(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	nearFull(t, s, 100)
	other.size = s.size
	other.Write([]byte(strings.Repeat("x", 90) + "\n"))
	stopRotation(t, s)
	s.Write([]byte(strings.Repeat("y", 50) + "\n")) // fits by what s counted alone
	if got := size(t, s.path()); got > rotateBytes {
		t.Errorf("the log is %d bytes, past the %d limit", got, rotateBytes)
	}
}

// And it can rotate the file: the gateway then follows, or it would write to an archive, and its
// next rotation would push the archives along with next to nothing in the newest.
func TestAFileRotatedByAnotherProcessIsFollowed(t *testing.T) {
	s, _ := failingSink(t)
	other, err := OpenSink(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	nearFull(t, s, 6)
	other.size = s.size
	other.Write([]byte("rotated by the other one\n"))
	s.Write([]byte("mine, too long for what was left\n"))
	got, _ := os.ReadFile(s.path())
	if string(got) != "rotated by the other one\nmine, too long for what was left\n" {
		t.Errorf("active: %q", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "events.2.jsonl")); err == nil {
		t.Error("rotated twice")
	}
	if got := size(t, filepath.Join(s.dir, "events.1.jsonl")); got != rotateBytes-6 {
		t.Errorf("archive 1 is %d bytes", got)
	}
}

// A crash between the two renames leaves the rotating file. It is the newest archive, not something
// to replace, in the main log and the audit log alike.
func TestAFileLeftMidRotationBecomesAnArchive(t *testing.T) {
	s, _ := failingSink(t)
	if err := os.WriteFile(s.rotatingPath(), []byte("left by a crash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.archive(1), []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nearFull(t, s, 6)
	s.Write([]byte("over the limit\n"))
	if got, _ := os.ReadFile(s.archive(2)); string(got) != "left by a crash\n" {
		t.Errorf("archive 2: %q", got)
	}
	if got, _ := os.ReadFile(s.archive(3)); string(got) != "older\n" {
		t.Errorf("archive 3: %q", got)
	}
	if got := size(t, s.archive(1)); got != rotateBytes-6 {
		t.Errorf("archive 1: %d bytes", got)
	}
	if _, err := os.Lstat(s.rotatingPath()); err == nil {
		t.Error("the rotating file is still there")
	}
}

// A planted link is the case most likely to name a path: it stays out of the complaint and the reason.
func TestAPlantedLinkDoesNotPutThePathInAComplaint(t *testing.T) {
	s, complaints := failingSink(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(s.dir, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	s.writeAudit([]byte("what someone did\n"))
	if strings.Contains(complaints.String(), s.dir) {
		t.Errorf("the complaint names the path: %q", complaints)
	}
	out := s.Outages()
	if len(out) != 1 || strings.Contains(out[0].Reason, s.dir) || out[0].Reason == "" {
		t.Errorf("outages: %+v", out)
	}
	if !strings.Contains(complaints.String(), "what someone did") {
		t.Errorf("the line was lost: %q", complaints)
	}
}

// doctor is where someone decides whether to act, so a copy on stderr doesn't read as safe.
func TestDescribeSaysHowLongTheStderrCopyLasts(t *testing.T) {
	line := LogOutage{Log: "audit", OnStderr: 2}.Describe(time.Now())
	if !strings.Contains(line, "2 kept only in the service's stderr") {
		t.Errorf("%q", line)
	}
	if survives := strings.Contains(line, "newest 1 MiB survives the next start"); survives != (goruntime.GOOS == "darwin") {
		t.Errorf("%q", line)
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
