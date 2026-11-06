package chaos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

const dlModel = "chaos-stt"

// artifactServer serves one model file, misbehaving as a test asks.
type artifactServer struct {
	URL  string
	data []byte
	srv  *http.Server

	mu sync.Mutex
	// behave serves request n (0-based). nil is an honest server.
	behave func(n int, w http.ResponseWriter, r *http.Request) bool
	ranges []string
}

func serveArtifact(t *testing.T, data []byte) *artifactServer {
	t.Helper()
	port, err := workspace.TakePort()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	a := &artifactServer{URL: fmt.Sprintf("http://127.0.0.1:%d", port), data: data}
	a.srv = &http.Server{Handler: a}
	go a.srv.Serve(ln)
	t.Cleanup(func() { a.srv.Close() })
	return a
}

func (a *artifactServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	n := len(a.ranges)
	a.ranges = append(a.ranges, r.Header.Get("Range"))
	behave := a.behave
	a.mu.Unlock()
	if behave != nil && behave(n, w, r) {
		return
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}

func (a *artifactServer) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ranges...)
}

// dropAfter sends part of the file with its full length, then closes the
// connection early. It is a plain close: on macOS a reset here can be lost on
// loopback, and the client then only learns from TCP keepalive 30 s later.
func dropAfter(w http.ResponseWriter, data []byte, part int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	w.Write(data[:part])
	w.(http.Flusher).Flush()
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

func modelBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	return data
}

// catalogEntry adds a small model served by a, with the file's real checksum.
func catalogEntry(a *artifactServer) func(rootDir string) error {
	sum := sha256.Sum256(a.data)
	return func(rootDir string) error {
		entry := map[string]any{
			"modelId": dlModel, "displayName": "Chaos", "kind": "stt", "provider": "LocalBase tests", "family": "Whisper",
			"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
			"source": a.URL + "/localbase/chaos", "repositoryRevision": strings.Repeat("c", 40),
			"artifacts": []any{map[string]any{"sourcePath": "ggml-chaos.bin", "filename": "ggml-chaos.bin",
				"expectedSizeBytes": len(a.data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
			"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
			"commercialStatus": "open", "catch": "Test data.", "notes": "Chaos fixture.",
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		// The artifact server is on loopback, which a catalog source may reach only by opt-in.
		st, err := store.Open(filepath.Join(rootDir, "local-base.db"))
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(rootDir, "catalog.d", dlModel+".json"), b, 0o600)
	}
}

type installState struct {
	Installed      bool    `json:"installed"`
	InstalledBytes float64 `json:"installedBytes"`
	Operation      *struct {
		State  string         `json:"state"`
		Detail string         `json:"detail"`
		Error  map[string]any `json:"error"`
	} `json:"operation"`
}

func modelState(t *testing.T, ctx context.Context, c *lab.Client, g *lab.Gateway) installState {
	t.Helper()
	r := c.Do(ctx, "GET", "/_localbase/model-management", g.Keys.Admin, "", nil)
	var list struct {
		Models []json.RawMessage `json:"models"`
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &list) != nil {
		t.Fatalf("model management: %d %.300s", r.Status, r.Body)
	}
	for _, raw := range list.Models {
		var id struct {
			ID string `json:"id"`
		}
		json.Unmarshal(raw, &id)
		if id.ID == dlModel {
			var s installState
			json.Unmarshal(raw, &s)
			return s
		}
	}
	t.Fatalf("%s is not listed", dlModel)
	return installState{}
}

func install(t *testing.T, ctx context.Context, c *lab.Client, g *lab.Gateway) {
	t.Helper()
	r := c.PostJSON(ctx, "/_localbase/model-management", g.Keys.Admin, map[string]any{"modelId": dlModel, "action": "install"})
	if r.Status != 202 {
		t.Fatalf("install: %d %.300s; logged: %s", r.Status, r.Body, managementFailures(t, g))
	}
}

// managementFailures is what the gateway logged about failed management requests.
func managementFailures(t *testing.T, g *lab.Gateway) string {
	t.Helper()
	b, err := os.ReadFile(g.LogPath)
	if err != nil {
		return err.Error()
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "models.management-failed") {
			out = append(out, line)
		}
	}
	if len(out) > 1 {
		out = out[len(out)-1:]
	}
	return strings.Join(out, "; ")
}

// installEnds waits for the operation to leave running and returns its state.
func installEnds(t *testing.T, ctx context.Context, c *lab.Client, g *lab.Gateway) installState {
	t.Helper()
	var s installState
	waitFor(t, "the install to end", 60*time.Second, func() bool {
		s = modelState(t, ctx, c, g)
		return s.Operation != nil && s.Operation.State != "running"
	})
	return s
}

func checkInstalledFile(t *testing.T, g *lab.Gateway, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(g.Root, "models", "stt", "ggml-chaos.bin"))
	if err != nil {
		t.Fatalf("the installed file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the installed file differs from the source (%d and %d bytes)", len(got), len(want))
	}
}

// A connection cut mid-download: the install fails and says so, keeps what it
// has, and the next install resumes from it and ends with the right bytes.
func TestDownloadDroppedThenResumed(t *testing.T) {
	data := modelBytes(8 << 20)
	a := serveArtifact(t, data)
	a.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 0 {
			dropAfter(w, data, len(data)/2)
			return true
		}
		return false
	}
	ctx, g, c := start(t, lab.Options{PrepareRoot: catalogEntry(a)})
	install(t, ctx, c, g)
	s := installEnds(t, ctx, c, g)
	if s.Operation.State != "failed" || s.Installed {
		t.Fatalf("a dropped download: %+v %+v", s, s.Operation)
	}
	if s.InstalledBytes < float64(len(data)/4) {
		t.Errorf("the partial file was not kept: %v bytes", s.InstalledBytes)
	}
	install(t, ctx, c, g)
	s = installEnds(t, ctx, c, g)
	if s.Operation.State != "complete" || !s.Installed {
		t.Fatalf("the second install: %+v %+v", s, s.Operation)
	}
	reqs := a.requests()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[1], "bytes=") {
		t.Errorf("the second install did not resume: %q", reqs)
	}
	checkInstalledFile(t, g, data)
}

// Bytes that do not match the checksum are never installed: the install fails,
// nothing bad stays behind to resume from, and an honest retry works.
func TestDownloadCorruptRefused(t *testing.T) {
	data := modelBytes(4 << 20)
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 0xFF
	a := serveArtifact(t, data)
	a.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 0 {
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(bad))
			return true
		}
		return false
	}
	ctx, g, c := start(t, lab.Options{PrepareRoot: catalogEntry(a)})
	install(t, ctx, c, g)
	s := installEnds(t, ctx, c, g)
	if s.Operation.State != "failed" || s.Installed {
		t.Fatalf("a corrupt download: %+v %+v", s, s.Operation)
	}
	dir := filepath.Join(g.Root, "models", "stt")
	if _, err := os.Stat(filepath.Join(dir, "ggml-chaos.bin")); err == nil {
		t.Error("the corrupt file was installed")
	}
	if _, err := os.Stat(filepath.Join(dir, "ggml-chaos.bin.partial")); err == nil {
		t.Error("the corrupt bytes stayed behind to resume from")
	}
	install(t, ctx, c, g)
	s = installEnds(t, ctx, c, g)
	if s.Operation.State != "complete" || !s.Installed {
		t.Fatalf("the retry: %+v %+v", s, s.Operation)
	}
	checkInstalledFile(t, g, data)
}

// A resume against a server that changed its mind about the file, or that
// ignores Range, never produces a wrong file.
func TestDownloadResumeAgainstBadServer(t *testing.T) {
	data := modelBytes(4 << 20)
	other := bytes.Clone(data)
	for i := range other {
		other[i] ^= 0x5A
	}
	cases := []struct {
		name   string
		second func(w http.ResponseWriter, r *http.Request)
	}{
		{"ignores Range and sends the whole file", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(200)
			w.Write(data)
		}},
		{"answers the Range from a changed file", func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(other))
		}},
		{"answers a Range it cannot satisfy", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := serveArtifact(t, data)
			a.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
				switch n {
				case 0:
					dropAfter(w, data, len(data)/2)
				case 1:
					tc.second(w, r)
				default:
					return false
				}
				return true
			}
			ctx, g, c := start(t, lab.Options{PrepareRoot: catalogEntry(a)})
			install(t, ctx, c, g)
			installEnds(t, ctx, c, g)
			install(t, ctx, c, g)
			s := installEnds(t, ctx, c, g)
			if s.Operation.State == "complete" {
				checkInstalledFile(t, g, data)
				return
			}
			if s.Installed {
				t.Errorf("failed yet installed: %+v", s)
			}
			// Whatever it left, an honest server finishes the job correctly.
			install(t, ctx, c, g)
			s = installEnds(t, ctx, c, g)
			if s.Operation.State != "complete" || !s.Installed {
				t.Fatalf("after the bad answer: %+v %+v", s, s.Operation)
			}
			checkInstalledFile(t, g, data)
		})
	}
}

// A peer that stays connected and stops sending: TCP keepalive cannot see it, so
// only an idle limit can. Without one the install runs forever, and because one
// install runs at a time, it blocks every other install until a restart.
func TestDownloadStalled(t *testing.T) {
	const limit = 30 * time.Second
	data := modelBytes(4 << 20)
	a := serveArtifact(t, data)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	a.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n != 0 {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	}
	ctx, g, c := start(t, lab.Options{PrepareRoot: catalogEntry(a), FakeEnv: []string{"LOCALBASE_DOWNLOAD_IDLE_TIMEOUT_MS=2000"}})
	install(t, ctx, c, g)
	var s installState
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if s = modelState(t, ctx, c, g); s.Operation != nil && s.Operation.State != "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.Operation == nil || s.Operation.State != "failed" {
		t.Fatalf("the install after %v of silence: %+v", limit, s.Operation)
	}
	if s.InstalledBytes < float64(len(data)/4) {
		t.Errorf("the partial file was not kept: %v bytes", s.InstalledBytes)
	}
	// It no longer blocks anything, and an honest retry resumes and ends correct.
	install(t, ctx, c, g)
	s = installEnds(t, ctx, c, g)
	if s.Operation.State != "complete" || !s.Installed {
		t.Fatalf("the install after the stall: %+v %+v", s, s.Operation)
	}
	checkInstalledFile(t, g, data)
	reqs := a.requests()
	if last := reqs[len(reqs)-1]; !strings.HasPrefix(last, "bytes=") {
		t.Errorf("the retry did not resume: %q", reqs)
	}
}

// --- a full disk -------------------------------------------------------------

// smallDisk gives a scratch volume of about 96 MiB: a disk image on macOS, or
// the tmpfs named by LOCALBASE_CHAOS_SMALL_DISK elsewhere (mount one with
// --tmpfs in a container, or with sudo on a CI runner).
func smallDisk(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("LOCALBASE_CHAOS_SMALL_DISK"); dir != "" {
		sub, err := os.MkdirTemp(dir, "chaos-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sub) })
		return sub
	}
	if goruntime.GOOS != "darwin" {
		t.Skip("no small volume: set LOCALBASE_CHAOS_SMALL_DISK to a small tmpfs")
	}
	base := t.TempDir()
	image := filepath.Join(base, "small.sparseimage")
	if out, err := exec.Command("hdiutil", "create", "-size", "96m", "-fs", "APFS", "-volname", "lblab", "-type", "SPARSE", image).CombinedOutput(); err != nil {
		t.Skipf("no disk image: %v %s", err, out)
	}
	mount := filepath.Join(base, "mnt")
	if err := os.Mkdir(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-noautoopen", "-mountpoint", mount, image).CombinedOutput(); err != nil {
		t.Skipf("no disk image: %v %s", err, out)
	}
	t.Cleanup(func() {
		workspace.KillStray()
		detach := func() ([]byte, error) { return exec.Command("hdiutil", "detach", "-force", mount).CombinedOutput() }
		if err := detachVolume(detach, func() { time.Sleep(500 * time.Millisecond) }, 20); err != nil {
			t.Errorf("detaching the scratch volume: %v", err)
		}
	})
	if real, err := filepath.EvalSymlinks(mount); err == nil {
		return real
	}
	return mount
}

// The most a volume may hold for fillDisk to fill it, and so the most it will ever write. A
// wrong LOCALBASE_CHAOS_SMALL_DISK must end in a failed test, not a full disk.
const fillDiskLimit = 1 << 30

// smallEnoughToFill says why dir is not on a small scratch volume, or nil if it is.
func smallEnoughToFill(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	if size := st.Blocks * uint64(st.Bsize); size > fillDiskLimit {
		return fmt.Errorf("%s is on a volume of %d MiB, and only a small scratch volume may be filled (at most %d MiB)", dir, size>>20, fillDiskLimit>>20)
	}
	return nil
}

// fillMargin is the free space fillDisk leaves. A completely full APFS image can refuse the delete
// that would free it: on macos-15 in CI the filler's delete was refused on all 50 tries, 5 s,
// after a fill to the last 4 KiB (#671). A delete needs room to update APFS's metadata, so the fill
// stops short. Why 4 MiB, measured on fresh 96 MB images like smallDisk's on macOS 26: a fill to
// ENOSPC there stops with 2.43 MiB still shown free, so this APFS keeps about that much back from
// writes, and margins of 64 KiB, 256 KiB and 1 MiB all ended at that same point and meant nothing;
// with 4 MiB the fill stopped at 4.0 MiB. 4 MiB is real free space where macos-15 keeps less back,
// and a delete's metadata needs a few 4 KiB nodes, far less.
const fillMargin = 4 << 20

// fillProbe is the write that shows the volume is full: one write larger than the margin, so it
// cannot fit whatever the exact free count is. APFS refuses such a write whole: in every trial it
// took 0 bytes, so proving the disk full costs the margin nothing.
const fillProbe = fillMargin + 1<<20

// freeBytes is what statfs says is free on dir's volume for an ordinary writer.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// fillDisk fills the small volume to fillMargin, proves it full with fillProbe, and returns how to
// free it.
func fillDisk(t *testing.T, dir string) (free func()) {
	t.Helper()
	if err := smallEnoughToFill(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "filler")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte{0xAA}, 1<<20)
	written := 0
	for {
		if written >= fillDiskLimit {
			t.Fatalf("wrote %d MiB and %s is still not full", written>>20, dir)
		}
		avail, err := freeBytes(dir)
		if err != nil {
			t.Fatal(err)
		}
		room := avail - fillMargin
		if room < 4096 {
			break
		}
		chunk := block
		if room < int64(len(block)) {
			chunk = block[:room/4096*4096] // whole 4 KiB blocks, down to the margin
		}
		n, err := f.Write(chunk)
		written += n
		if err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				break // the volume refused before the margin, as APFS does with some still free
			}
			t.Fatalf("filling the disk: %v (%d bytes free)", err, avail)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("syncing the filler: %v", err)
	}
	f.Close()
	if err := probeFull(dir); err != nil {
		t.Fatalf("filled to the margin, and %v", err)
	}
	freed := false
	return func() {
		if !freed {
			freed = true
			ops := diskOps{
				truncate:  os.Truncate,
				remove:    os.Remove,
				available: func() (int64, error) { return freeBytes(dir) },
				wait:      func() { time.Sleep(100 * time.Millisecond) },
			}
			if err := freeFullDisk(path, int64(written), ops, 50); err != nil {
				t.Errorf("freeing the disk: %v", err)
			}
		}
	}
}

// probeFull shows that dir's volume is full: one write of fillProbe bytes must be refused. The
// probe file is deleted either way, while the margin is still there to delete it in.
func probeFull(dir string) error {
	before, err := freeBytes(dir)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "probe")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("the probe could not be created: %w (%d bytes free)", err, before)
	}
	n, werr := f.Write(make([]byte, fillProbe))
	f.Close()
	after, _ := freeBytes(dir)
	if rerr := os.Remove(p); rerr != nil {
		return fmt.Errorf("the probe took %d bytes and could not be deleted: %w (%d bytes free before, %d after)", n, rerr, before, after)
	}
	switch {
	case werr == nil:
		return fmt.Errorf("a write of %d bytes fit, so it is not full (%d bytes free before it)", fillProbe, before)
	case !errors.Is(werr, syscall.ENOSPC):
		return fmt.Errorf("the probe failed otherwise: %w (%d bytes free)", werr, before)
	}
	return nil
}

// diskOps are what freeFullDisk does to the disk, so a test can make each of them fail.
type diskOps struct {
	truncate  func(path string, size int64) error
	remove    func(path string) error
	available func() (int64, error)
	wait      func()
}

const (
	// givenBack is how much of the filler goes when a delete is refused. freedFloor is what must be
	// free once it has gone.
	givenBack  = 1 << 20
	freedFloor = 256 << 10
)

// freeFullDisk frees a full volume. With fillMargin left, the first delete is expected to work; the
// rest is a bounded fallback for a volume something else filled the rest of the way, as the
// gateway's own failing writes can. A completely full APFS image can refuse the very delete that
// would free it (remove: no space left on device), and can refuse to truncate for longer than it
// refuses to delete, so the delete comes first and giving a little back is what it tries between
// refusals. Each refusal is recorded with the free bytes at that moment, so one failure in CI says
// what the volume did. Then the room must really have come back, or the test after it would fail
// for a reason nobody could see.
func freeFullDisk(path string, size int64, ops diskOps, tries int) error {
	removed := false
	var err error
	var refusals []string
	for range tries {
		if err = ops.remove(path); err == nil {
			removed = true
			break
		}
		free, ferr := ops.available()
		note := fmt.Sprintf("%d bytes free: %v", free, err)
		if ferr != nil {
			note = fmt.Sprintf("free unknown (%v): %v", ferr, err)
		}
		refusals = append(refusals, note)
		if !errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("%w (%s)", err, note)
		}
		if err := ops.truncate(path, max(size-givenBack, 0)); err != nil && !errors.Is(err, syscall.ENOSPC) {
			return err
		}
		ops.wait()
	}
	if !removed {
		return fmt.Errorf("deleting the filler: still refused after %d tries; each try: %s: %w", tries, runs(refusals), err)
	}
	var free int64
	for range tries {
		if free, err = ops.available(); err != nil {
			return err
		}
		if free >= freedFloor {
			return nil
		}
		ops.wait()
	}
	return fmt.Errorf("deleted the filler, and only %d bytes are free, under the floor of %d", free, freedFloor)
}

// runs puts repeated notes together, so fifty refusals of the same kind read as one line.
func runs(notes []string) string {
	var out []string
	for i := 0; i < len(notes); {
		j := i
		for j < len(notes) && notes[j] == notes[i] {
			j++
		}
		if j-i == 1 {
			out = append(out, fmt.Sprintf("try %d: %s", i+1, notes[i]))
		} else {
			out = append(out, fmt.Sprintf("tries %d-%d: %s", i+1, j, notes[i]))
		}
		i = j
	}
	return strings.Join(out, "; ")
}

// detachVolume detaches a scratch image, waiting a bounded time while macOS still has it busy, as it
// can straight after a fill and delete: hdiutil: couldn't eject "disk4" - Resource busy, on macos-14,
// even with -force, on an image nothing else had touched.
func detachVolume(detach func() ([]byte, error), wait func(), tries int) error {
	var out []byte
	var err error
	for range tries {
		if out, err = detach(); err == nil {
			return nil
		}
		if !strings.Contains(string(out), "Resource busy") {
			return fmt.Errorf("%w: %s", err, out)
		}
		wait()
	}
	return fmt.Errorf("still busy after %d tries: %w: %s", tries, err, out)
}

// fillDisk must refuse an ordinary directory, before writing anything to it.
func TestFillDiskRefusesAnOrdinaryVolume(t *testing.T) {
	dir := t.TempDir()
	if err := smallEnoughToFill(dir); err == nil {
		t.Skipf("%s really is on a small volume", dir)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("wrote to it: %v %v", entries, err)
	}
}

// And it fills a real small volume, within the limit, and frees it.
func TestFillDiskFillsASmallVolume(t *testing.T) {
	dir := smallDisk(t)
	if err := smallEnoughToFill(dir); err != nil {
		t.Fatal(err)
	}
	free := fillDisk(t, dir)
	if avail, err := freeBytes(dir); err != nil || avail > fillMargin+(1<<20) {
		t.Errorf("filled, %d bytes are still free (%v), more than the margin of %d and a MiB", avail, err, fillMargin)
	}
	// Full means a write larger than the margin is refused, whatever the exact free count.
	if err := os.WriteFile(filepath.Join(dir, "more"), make([]byte, fillProbe), 0o600); err == nil {
		avail, _ := freeBytes(dir)
		t.Errorf("a full volume took a write of %d bytes (%d bytes free after it)", fillProbe, avail)
	}
	os.Remove(filepath.Join(dir, "more"))
	free()
	if err := os.WriteFile(filepath.Join(dir, "more"), make([]byte, 1<<20), 0o600); err != nil {
		avail, _ := freeBytes(dir)
		t.Errorf("freed, it still refuses: %v (%d bytes free)", err, avail)
	}
}

// The disk fills during a download: the install fails and says so, the gateway
// keeps serving, and once there is room the install resumes and ends correct, with
// no restart.
//
// Whether a full disk also stops the lease's mutation lock depends on what space
// the volume happens to have left (the bug showed in 4 of 6 runs), so the test
// takes that lock itself while the install is failing. The release then cannot
// get it, every time, as on a disk with no room at all.
func TestDiskFullDuringDownload(t *testing.T) {
	disk := smallDisk(t)
	data := modelBytes(32 << 20)
	release := make(chan struct{})
	a := serveArtifact(t, data)
	a.behave = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n != 0 {
			return false
		}
		// Half arrives; the disk fills; then the rest cannot be written.
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		<-release
		w.Write(data[len(data)/2:])
		return true
	}
	ctx, g, c := start(t, lab.Options{Dir: filepath.Join(disk, "gw"), PrepareRoot: catalogEntry(a)})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	install(t, ctx, c, g)
	waitFor(t, "half the file", 30*time.Second, func() bool {
		return modelState(t, ctx, c, g).InstalledBytes >= float64(len(data)/2)
	})
	lease, mutation := g.LeasePaths()
	if _, err := os.Stat(lease); err != nil {
		t.Fatalf("the install holds no lease at the expected path: %v", err)
	}
	if err := os.Mkdir(mutation, 0o700); err != nil {
		t.Fatal(err)
	}
	holder, _ := json.Marshal(map[string]any{"version": 1, "token": "chaos", "pid": g.Cmd.Process.Pid})
	if err := os.WriteFile(filepath.Join(mutation, "owner.json"), holder, 0o600); err != nil {
		t.Fatal(err)
	}
	free := fillDisk(t, disk)
	defer free()
	close(release)
	s := installEnds(t, ctx, c, g)
	if s.Operation.State != "failed" || s.Installed {
		t.Fatalf("a download onto a full disk: %+v %+v", s, s.Operation)
	}
	if d := strings.ToLower(s.Operation.Detail); !strings.Contains(d, "storage") && !strings.Contains(d, "space") {
		t.Errorf("the failure does not point at storage: %q", s.Operation.Detail)
	}
	mustChat(t, ctx, c, g, lab.ChatSmall)
	if err := os.RemoveAll(mutation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease); err == nil {
		t.Errorf("the failed install still holds the root's lease")
	}
	free()
	// The volume may take a moment to show the space. Until then a write can still
	// fail, and the answer is the transient one; a client that honors Retry-After
	// gets through.
	var retried lab.Call
	waitFor(t, "an install to be accepted after freeing the disk", 20*time.Second, func() bool {
		retried = c.PostJSON(ctx, "/_localbase/model-management", g.Keys.Admin, map[string]any{"modelId": dlModel, "action": "install"})
		if retried.Status != 202 {
			noServerError(t, "an install just after freeing the disk", retried)
			t.Logf("install %d (Retry-After %q); logged: %s", retried.Status, retried.RetryAfter, managementFailures(t, g))
			time.Sleep(500 * time.Millisecond)
		}
		return retried.Status == 202
	})
	s = installEnds(t, ctx, c, g)
	if s.Operation.State != "complete" || !s.Installed {
		t.Fatalf("the install after freeing space: %+v %+v", s, s.Operation)
	}
	checkInstalledFile(t, g, data)
}

// A disk that fills while the gateway serves: requests keep working, what needs
// a write says it cannot, nothing panics or exits, and it all comes back when
// there is room.
func TestDiskFullWhileServing(t *testing.T) {
	disk := smallDisk(t)
	ctx, g, c := start(t, lab.Options{Dir: filepath.Join(disk, "gw")})
	mustChat(t, ctx, c, g, lab.ChatSmall)
	free := fillDisk(t, disk)
	defer free()
	for i := range 50 {
		r := c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello"))
		if r.Err != nil || r.Status != 200 {
			t.Fatalf("chat %d on a full disk: %d %v %.300s", i, r.Status, r.Err, r.Body)
		}
	}
	if r := c.Do(ctx, "GET", "/health", "", "", nil); r.Status != 200 {
		t.Errorf("health on a full disk: %d", r.Status)
	}
	// A write that cannot happen is refused, never a panic or a false success.
	create := c.PostJSON(ctx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{"action": "create", "name": "on-a-full-disk", "scopes": []string{"models:read"}})
	noServerError(t, "creating a key on a full disk", create)
	madeOnFullDisk := create.Status < 300
	run := c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", g.Keys.Runs[0], chatBody(lab.ChatSmall, "fake:reply on a full disk"))
	noServerError(t, "a run on a full disk", run)
	select {
	case <-g.Done():
		t.Fatalf("the gateway exited on a full disk: %v", g.ExitErr())
	default:
	}
	free()
	mustChat(t, ctx, c, g, lab.ChatSmall)
	again := c.PostJSON(ctx, "/_localbase/api-keys", g.Keys.Admin, map[string]any{"action": "create", "name": "after-the-disk-freed", "scopes": []string{"models:read"}})
	if again.Status >= 300 {
		t.Errorf("creating a key after freeing the disk: %d %.300s", again.Status, again.Body)
	}
	t.Logf("key creation on a full disk: %d (succeeded: %v); a run: %d", create.Status, madeOnFullDisk, run.Status)
}

// --- many sessions -----------------------------------------------------------

// A thousand browser sessions in the database: finding one stays cheap, under
// concurrent lookups and with the gateway running on the same file. This goes
// through the session store, not HTTP; the browser suite covers the sign-in path.
func TestThousandSessions(t *testing.T) {
	ctx, g, _ := start(t, lab.Options{})
	st, err := store.Open(dbPath(g))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const issuer = "https://chaos.invalid"
	identity := auth.Identity{Issuer: issuer, Subject: "chaos"}
	tokens := make([]string, 0, 1000)
	for range 1000 {
		token, err := auth.CreateSession(ctx, st, "owner", identity)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	issuers := map[string]bool{issuer: true}
	var mu sync.Mutex
	var took []time.Duration
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 250 {
				began := time.Now()
				s, err := auth.FindSession(ctx, st, tokens[(worker*250+i)%len(tokens)], issuers)
				d := time.Since(began)
				mu.Lock()
				took = append(took, d)
				if err != nil || s == nil {
					t.Errorf("session lookup: %v %v", s, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	p50, p99 := percentiles(took)
	t.Logf("session lookup with 1000 sessions, 4 concurrent: p50 %v p99 %v", p50, p99)
	if p99 > 500*time.Millisecond {
		t.Errorf("session lookup p99 %v", p99)
	}
	if s, err := auth.FindSession(ctx, st, "not-a-token", issuers); err == nil && s != nil {
		t.Errorf("an unknown token found a session: %+v", s)
	}
}
