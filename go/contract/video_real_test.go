package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// TestVideoReal runs one FastWan text-to-video job at the catalog's
// qualification bounds on each implementation, TS first, during an
// operator-approved memory window (LOCALBASE_CONTRACT_VIDEO_MODELS names
// read-only clones of the weights; LOCALBASE_CONTRACT_VIDEO_OUT keeps the
// outputs and report). Each gateway is halted, with its sd-server, as soon
// as its job ends. While a job runs, host memory, the sd-server footprint,
// the live gateway's /health and its warnings are watched: available memory
// below 12 GiB, or a live memory refusal or pressure change, cancels the job
// and stops the run.
func TestVideoReal(t *testing.T) {
	if os.Getenv("LOCALBASE_CONTRACT_VIDEO_MODELS") == "" {
		t.Skip("set LOCALBASE_CONTRACT_VIDEO_MODELS for the real video run (operator memory window)")
	}
	out := os.Getenv("LOCALBASE_CONTRACT_VIDEO_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o700)
	byName := map[string]*Target{}
	for _, tg := range targets {
		byName[tg.Name] = tg
	}
	host := memory.NewHostProvider()
	report := map[string]any{}
	for _, name := range []string{"ts", "go"} {
		tg := byName[name]
		if tg == nil {
			continue
		}
		r := runRealVideo(t, tg, host, out)
		report[name] = r
		b, _ := json.MarshalIndent(report, "", "  ")
		os.WriteFile(filepath.Join(out, "report.json"), b, 0o600)
		if t.Failed() {
			return
		}
	}
	// Compare the outputs with the pinned ffmpeg.
	probe := map[string]map[string]string{}
	for name := range report {
		probe[name] = probeVideo(t, filepath.Join(out, name+".mp4"))
		report[name].(map[string]any)["probe"] = probe[name]
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	os.WriteFile(filepath.Join(out, "report.json"), b, 0o600)
	t.Logf("report:\n%s", b)
	if len(probe) == 2 {
		for _, k := range []string{"container", "codec", "frames", "width", "height", "fps"} {
			if probe["ts"][k] != probe["go"][k] {
				t.Errorf("%s differs: ts %q, go %q", k, probe["ts"][k], probe["go"][k])
			}
		}
	}
}

const liveGateway = "http://127.0.0.1:2273"

func runRealVideo(t *testing.T, tg *Target, host memory.HostProvider, out string) map[string]any {
	t.Helper()
	ctx := context.Background()
	gib := func(b int64) float64 { return float64(b) / float64(memory.GiB) }
	// Preflight: 25 GiB available and the live gateway healthy.
	if a := host.Snapshot(ctx).Available; a < 25*memory.GiB {
		t.Fatalf("%s: preflight: %.1f GiB available, need 25", tg.Name, gib(a))
	}
	if !liveHealthy() {
		t.Fatalf("%s: preflight: the live gateway is not healthy", tg.Name)
	}
	started := time.Now()
	body := `{"model":"fastwan2.2-ti2v-5b-q6_k","prompt":"a red paper boat drifting on a calm pond, soft morning light","width":480,"height":832,"frames":81,"fps":16,"input":{"kind":"text"}}`
	h := bearer(tg.Key)
	h["Content-Type"] = "application/json"
	res := do(t, "POST", tg.BaseURL+"/v1/videos", h, body)
	if res.Status != 202 {
		t.Fatalf("%s: create %d %s", tg.Name, res.Status, res.Body)
	}
	id := res.JSON(t)["id"].(string)

	var mu sync.Mutex
	minAvail, peakFootprint, healthFailures := int64(1<<62), int64(0), 0
	abort := ""
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			n++
			a := host.Snapshot(ctx).Available
			var fp int64
			for _, pid := range sdServerPIDs(tg.Root) {
				if b, ok := memory.Footprint(ctx, pid); ok {
					fp += b
				}
			}
			healthy := liveHealthy()
			reason := ""
			if a >= 0 && a < 12*memory.GiB {
				reason = fmt.Sprintf("available memory %.1f GiB is below 12 GiB", gib(a))
			}
			if n%5 == 0 {
				if ev := liveMemoryWarnings(started); ev != "" {
					reason = "the live gateway logged " + ev
				}
			}
			mu.Lock()
			if a >= 0 && a < minAvail {
				minAvail = a
			}
			peakFootprint = max(peakFootprint, fp)
			if !healthy {
				healthFailures++
			}
			if reason != "" && abort == "" {
				abort = reason
			}
			mu.Unlock()
			if reason != "" {
				return
			}
		}
	}()
	var final map[string]any
	for {
		mu.Lock()
		why := abort
		mu.Unlock()
		if why != "" {
			do(t, "POST", tg.BaseURL+"/v1/videos/"+id+"/cancel", bearer(tg.Key), "")
			tg.halt()
			killSdServers(tg.Root)
			t.Fatalf("%s: stopped: %s", tg.Name, why)
		}
		s := do(t, "GET", tg.BaseURL+"/v1/videos/"+id, bearer(tg.Key), "").JSON(t)
		if s["status"] != "queued" && s["status"] != "in_progress" {
			final = s
			break
		}
		if time.Since(started) > 12*time.Minute {
			do(t, "POST", tg.BaseURL+"/v1/videos/"+id+"/cancel", bearer(tg.Key), "")
			tg.halt()
			killSdServers(tg.Root)
			t.Fatalf("%s: job still %v after 12 minutes", tg.Name, s["status"])
		}
		time.Sleep(time.Second)
	}
	wall := time.Since(started)
	content := do(t, "GET", tg.BaseURL+"/v1/videos/"+id+"/content", bearer(tg.Key), "")
	close(stop)
	<-watched
	tg.halt() // the gateway and its sd-server stop as soon as the job ends
	left := killSdServers(tg.Root)
	mu.Lock()
	defer mu.Unlock()
	r := map[string]any{"job": final, "wall_seconds": wall.Seconds(), "min_available_gib": gib(minAvail),
		"peak_sd_server_footprint_gib": gib(peakFootprint), "live_health_failures": healthFailures,
		"sd_servers_killed_after_halt": left, "content_status": content.Status, "content_bytes": len(content.Body),
		"content_type": content.Header.Get("Content-Type")}
	if final["status"] != "completed" || content.Status != 200 {
		t.Errorf("%s: %v (content %d)", tg.Name, final, content.Status)
		return r
	}
	os.WriteFile(filepath.Join(out, tg.Name+".mp4"), content.Body, 0o600)
	t.Logf("%s: %.0f s, min available %.1f GiB, peak sd-server %.1f GiB, live health failures %d",
		tg.Name, wall.Seconds(), gib(minAvail), gib(peakFootprint), healthFailures)
	return r
}

func liveHealthy() bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(liveGateway + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// liveMemoryWarnings reads the live gateway's logs (read-only) for a memory
// refusal or pressure change since the run started.
func liveMemoryWarnings(since time.Time) string {
	home, _ := os.UserHomeDir()
	cmd := exec.Command(filepath.Join(home, ".local/bin/local-base"), "logs", "--json", "--limit", "500",
		"--since", since.UTC().Format("2006-01-02T15:04:05.000Z"))
	b, _ := cmd.Output()
	for _, ev := range []string{"runtime.memory-admission-rejected", "runtime.memory-pressure-changed"} {
		if strings.Contains(string(b), `"`+ev+`"`) {
			return ev
		}
	}
	return ""
}

// sdServerPIDs finds the sd-server processes serving this scratch root.
func sdServerPIDs(root string) []int {
	b, _ := exec.Command("ps", "-ax", "-o", "pid=,command=").Output()
	var pids []int
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.Contains(line, "sd-server") || !strings.Contains(line, root) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killSdServers stops any sd-server of this scratch root still running
// after its gateway halted, and reports how many there were.
func killSdServers(root string) int {
	pids := sdServerPIDs(root)
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	if len(pids) > 0 {
		time.Sleep(3 * time.Second)
		for _, pid := range sdServerPIDs(root) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return len(pids)
}

// probeVideo reads container, codec, size, rate, frames and duration with
// the pinned ffmpeg.
func probeVideo(t *testing.T, path string) map[string]string {
	t.Helper()
	exe, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALBASE_CONTRACT_RUNTIMES"), "ffmpeg", "*", "ffmpeg"))
	if len(exe) == 0 {
		t.Errorf("no pinned ffmpeg to probe %s", path)
		return nil
	}
	b, _ := os.ReadFile(path)
	out, _ := exec.Command(exe[0], "-hide_banner", "-nostdin", "-i", path, "-map", "0:v:0", "-c", "copy", "-f", "null", "-").CombinedOutput()
	s := string(out)
	get := func(re string) string {
		if m := regexp.MustCompile(re).FindStringSubmatch(s); m != nil {
			return m[1]
		}
		return ""
	}
	p := map[string]string{"bytes": strconv.Itoa(len(b)), "duration": get(`Duration: ([0-9:.]+)`),
		"codec": get(`Video: (\w+)`), "width": get(`, (\d+)x\d+`), "height": get(`, \d+x(\d+)`),
		"fps": get(`, ([\d.]+) fps`), "frames": get(`frame=\s*(\d+)\s+fps=[^\n]*\n?[^\n]*$`)}
	if p["frames"] == "" {
		frames := regexp.MustCompile(`frame=\s*(\d+)`).FindAllStringSubmatch(s, -1)
		if len(frames) > 0 {
			p["frames"] = frames[len(frames)-1][1]
		}
	}
	if len(b) >= 8 {
		p["container"] = string(b[4:8])
	}
	return p
}
