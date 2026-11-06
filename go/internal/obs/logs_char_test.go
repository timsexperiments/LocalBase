package obs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Characterization, inventory item 4: log rotation and the service stderr bound.
// Nothing exercised either. It pins today's behaviour.

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// nearFull grows the active log (sparsely) to just under the rotation size.
func nearFull(t *testing.T, s *Sink, room int64) {
	t.Helper()
	if err := os.Truncate(s.path(), rotateBytes-room); err != nil {
		t.Fatal(err)
	}
	s.size = rotateBytes - room
}

func TestSinkRotation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m := perm(t, dir); m != 0o700 {
		t.Errorf("directory: %o", m)
	}
	if m := perm(t, s.path()); m != 0o600 {
		t.Errorf("file: %o", m)
	}
	// A line that exactly fills the file stays; the next one rotates.
	nearFull(t, s, 6)
	s.Write([]byte("fits\n"))
	if _, err := os.Stat(filepath.Join(dir, "events.1.jsonl")); err == nil {
		t.Fatal("rotated before the limit")
	}
	s.size = rotateBytes - 1
	s.Write([]byte("second\n"))
	archive := filepath.Join(dir, "events.1.jsonl")
	if m := perm(t, archive); m != 0o600 {
		t.Errorf("archive: %o", m)
	}
	if got, _ := os.ReadFile(s.path()); string(got) != "second\n" {
		t.Errorf("active after rotating: %q", got)
	}
	if got, _ := os.ReadFile(archive); !strings.HasSuffix(string(got), "fits\n") {
		t.Errorf("the archive lost the line before the rotation: %d bytes", len(got))
	}
}

// Five archives are kept, newest first, and the oldest falls off.
func TestSinkKeepsFiveArchives(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const kept = 5 // written out: a change to the constant must fail here
	for i := 1; i <= kept+3; i++ {
		s.Write(fmt.Appendf(nil, "generation %d\n", i))
		s.size = rotateBytes // the next write rotates
	}
	s.Write([]byte("newest\n"))
	if got, _ := os.ReadFile(s.path()); string(got) != "newest\n" {
		t.Errorf("active: %q", got)
	}
	for i := 1; i <= kept; i++ {
		want := fmt.Sprintf("generation %d\n", kept+3-i+1)
		if got, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("events.%d.jsonl", i))); err != nil || string(got) != want {
			t.Errorf("archive %d: %q %v, want %q", i, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("events.%d.jsonl", kept+1))); err == nil {
		t.Error("kept more archives than the limit")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != kept+1 {
		t.Errorf("%d files", len(entries))
	}
}

// A restart continues the file, and counts what is already there.
func TestSinkReopenCountsExistingSize(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("before\n"))
	s.Close()
	s, err = OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.size != 7 {
		t.Errorf("size after reopening: %d", s.size)
	}
	s.Write([]byte("after\n"))
	if got, _ := os.ReadFile(s.path()); string(got) != "before\nafter\n" {
		t.Errorf("%q", got)
	}
	nearFull(t, s, 1)
	s.Close()
	s, err = OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Write([]byte("next\n"))
	if _, err := os.Stat(filepath.Join(dir, "events.1.jsonl")); err != nil {
		t.Errorf("a nearly full file from before the restart did not rotate: %v", err)
	}
}

// A single line larger than the limit is still written whole, not lost.
func TestSinkOversizedLine(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	big := bytes.Repeat([]byte("x"), rotateBytes+10)
	s.Write(big)
	if info, _ := os.Stat(s.path()); info.Size() != int64(len(big)) {
		t.Errorf("active: %d bytes", info.Size())
	}
}

// Lines from many goroutines are never interleaved or lost, across rotations.
func TestSinkConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	nearFull(t, s, 200)
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				s.Write(fmt.Appendf(nil, "w%d-%d\n", w, i))
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, name := range []string{"events.jsonl", "events.1.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(bytes.Trim(raw, "\x00")), "\n") {
			if line = strings.Trim(line, "\x00"); line != "" {
				seen[line]++
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "events.1.jsonl")); err != nil {
		t.Fatal("the run should have crossed a rotation")
	}
	for w := range writers {
		for i := range each {
			if key := fmt.Sprintf("w%d-%d", w, i); seen[key] != 1 {
				t.Errorf("%s written %d times", key, seen[key])
			}
		}
	}
}

func TestSinkMirror(t *testing.T) {
	s, err := OpenSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mirror bytes.Buffer
	s.Mirror = &mirror
	s.Write([]byte("seen\n"))
	if mirror.String() != "seen\n" {
		t.Errorf("%q", mirror.String())
	}
}

func stderrFixture(t *testing.T, size int) (root, active, archive string) {
	t.Helper()
	root = t.TempDir()
	active, archive = stderrPaths(root)
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}
	if size >= 0 {
		b := bytes.Repeat([]byte("o"), size)
		// Marks the two ends, to see which part survives.
		if size > 8 {
			copy(b, "OLDEST__")
			copy(b[size-8:], "NEWEST__")
		}
		if err := os.WriteFile(active, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, active, archive
}

func TestBoundServiceStderr(t *testing.T) {
	// Nothing captured: nothing to do.
	root, _, archive := stderrFixture(t, -1)
	if err := BoundServiceStderr(root); err != nil {
		t.Errorf("missing: %v", err)
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("archive from nothing")
	}
	// At the limit: left alone.
	root, active, archive := stderrFixture(t, maxServiceStderr)
	if err := BoundServiceStderr(root); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(active); info.Size() != maxServiceStderr {
		t.Errorf("at the limit: %d", info.Size())
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("archived at the limit")
	}
	// Over it: the newest 1 MiB moves to the private archive, and the active file
	// empties, in place, since launchd holds it open.
	root, active, archive = stderrFixture(t, maxServiceStderr+100)
	before, _ := os.Stat(active)
	if err := BoundServiceStderr(root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(active)
	if after.Size() != 0 || !os.SameFile(before, after) {
		t.Errorf("active: %d bytes, same file %v", after.Size(), os.SameFile(before, after))
	}
	got, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxServiceStderr || !bytes.HasSuffix(got, []byte("NEWEST__")) || bytes.Contains(got, []byte("OLDEST__")) {
		t.Errorf("archive: %d bytes", len(got))
	}
	if m := perm(t, archive); m != 0o600 {
		t.Errorf("archive: %o", m)
	}
	// A later bound replaces the archive; only one is kept.
	if err := os.WriteFile(active, bytes.Repeat([]byte("n"), maxServiceStderr+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BoundServiceStderr(root); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(archive)
	if bytes.Contains(got, []byte("NEWEST__")) {
		t.Error("the older archive survived")
	}
	entries, _ := os.ReadDir(filepath.Dir(active))
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestInspectServiceStderr(t *testing.T) {
	root, active, archive := stderrFixture(t, -1)
	if got := InspectServiceStderr(root); got != nil {
		t.Errorf("nothing captured: %+v", got)
	}
	os.WriteFile(active, nil, 0o600)
	if got := InspectServiceStderr(root); got != nil {
		t.Errorf("an empty capture: %+v", got)
	}
	os.WriteFile(active, []byte("private stderr text"), 0o600)
	got := InspectServiceStderr(root)
	if got == nil || got.Bytes != 19 || got.ArchiveBytes != 0 || got.Path != active || got.ModifiedAt == "" {
		t.Fatalf("active only: %+v", got)
	}
	os.WriteFile(archive, []byte("old"), 0o600)
	os.Truncate(active, 0)
	if got := InspectServiceStderr(root); got == nil || got.Bytes != 0 || got.ArchiveBytes != 3 {
		t.Errorf("archive only: %+v", got)
	}
	// A symlink is not a capture: never followed, and its size not reported.
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, []byte("not ours"), 0o600)
	os.Remove(active)
	os.Remove(archive)
	os.Symlink(target, active)
	if got := InspectServiceStderr(root); got != nil {
		t.Errorf("through a symlink: %+v", got)
	}
}
