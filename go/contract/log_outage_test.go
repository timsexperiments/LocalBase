package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A real gateway whose log can't rotate: doctor and status say so, in words and in JSON, and the
// file stays where it was.
func TestDoctorAndStatusReportALogThatCannotRotate(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	created := e.json("keys", "create", "--name", "reader", "--scopes", "models:read")
	secret, _ := created["secret"].(string)
	if secret == "" {
		t.Fatalf("no secret: %v", created)
	}
	logDir := filepath.Join(e.root, "logs")
	active := filepath.Join(logDir, "events.jsonl")
	if err := fsguard.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const limit = 10 << 20
	// The commands above write their audit lines here (#608), so the file usually exists already, and
	// guardedSparse leaves an existing file alone: it only makes a missing one. mustGrow grows it to
	// just under the limit either way.
	if err := fsguard.Grow(active, limit-64<<10); err != nil {
		t.Fatal(err)
	}
	e.serve()
	defer e.stop()
	// The gateway makes its directories private when it starts, so this has to come after.
	if err := fsguard.Chmod(logDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsguard.Chmod(logDir, 0o700) })
	full := func() bool {
		info, err := os.Stat(active)
		return err == nil && info.Size() > limit-4<<10
	}
	for range 5000 { // Requests are logged, until the next line would pass the limit.
		if full() {
			break
		}
		req, _ := http.NewRequest("GET", e.base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if !full() {
		t.Fatal("the requests never filled the log")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // Keep something arriving so that the next line is the one over the limit.
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			req, _ := http.NewRequest("GET", e.base+"/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}
	}()
	type outage struct {
		Log       string `json:"log"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
		Reason    string `json:"reason"`
		Since     string `json:"since"`
	}
	check := func(command string) {
		t.Helper()
		var last string
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			r := e.cli(nil, "--json", command)
			last = r.stdout
			var got struct {
				Data struct {
					LogOutages []outage `json:"logOutages"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(r.stdout), &got) == nil && len(got.Data.LogOutages) > 0 {
				o := got.Data.LogOutages[0]
				if o.Log != "events" || !strings.HasSuffix(o.Path, filepath.Join("logs", "events.jsonl")) || o.SizeBytes < limit-4<<10 || o.SizeBytes > limit ||
					!strings.Contains(o.Reason, "permission denied") || o.Since == "" {
					t.Errorf("%s: %+v", command, o)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never reported it: %.400s", command, last)
			}
		}
		if plain := e.cli(nil, command); !strings.Contains(plain.stdout+plain.stderr, "Log problem: the events log at ") {
			t.Errorf("%s in words: %.600s", command, plain.stdout+plain.stderr)
		}
	}
	check("status")
	check("doctor")
	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > limit {
		t.Errorf("the log grew past its limit: %d", info.Size())
	}
}
