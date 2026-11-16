package chaos

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/lab"
)

// What fake:die-saying prints before it exits 1.
const crashLine = "fake: ran out of device memory at layer 7"

// A crash leaves its reason behind, through the real path: a real gateway, a backend that
// says why and exits 1, and a real diagnostics bundle. What the backend printed is in the
// log and the bundle at debug level; how it exited is there at the default level too.
func TestCrashReasonReachesLogAndBundle(t *testing.T) {
	crash := map[string]func(context.Context, *lab.Client, *lab.Gateway) lab.Call{
		"llm": func(ctx context.Context, c *lab.Client, g *lab.Gateway) lab.Call {
			return c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "fake:die-saying"))
		},
		"image": func(ctx context.Context, c *lab.Client, g *lab.Gateway) lab.Call {
			return c.PostJSON(ctx, "/v1/images/generations", g.Keys.User, map[string]any{"model": lab.Image, "prompt": "fake:die-saying", "n": 1, "size": "512x512"})
		},
	}
	warm := map[string]func(context.Context, *lab.Client, *lab.Gateway) lab.Call{
		"llm": func(ctx context.Context, c *lab.Client, g *lab.Gateway) lab.Call {
			return c.PostJSON(ctx, chatPath, g.Keys.User, chatBody(lab.ChatSmall, "hello"))
		},
		"image": func(ctx context.Context, c *lab.Client, g *lab.Gateway) lab.Call {
			return c.PostJSON(ctx, "/v1/images/generations", g.Keys.User, map[string]any{"model": lab.Image, "prompt": "a boat", "n": 1, "size": "512x512"})
		},
	}
	for _, runtime := range []string{"llm", "image"} {
		for _, level := range []string{"debug", "info"} {
			t.Run(runtime+"_"+level, func(t *testing.T) {
				ctx, g, c := start(t, lab.Options{LogLevel: level})
				if r := warm[runtime](ctx, c, g); r.Status != 200 {
					t.Fatalf("before the crash: %d %.300s", r.Status, r.Body)
				}
				crash[runtime](ctx, c, g)
				waitFor(t, "the crash to be logged", 15*time.Second, func() bool {
					return len(events(t, g, "runtime.exited")) > 0
				})
				log, err := os.ReadFile(g.LogPath)
				if err != nil {
					t.Fatal(err)
				}
				check := func(where string, text []byte) {
					t.Helper()
					if !exitReasonIn(t, text, runtime) {
						t.Errorf("%s: no runtime.exited for %s with the exit status", where, runtime)
					}
					if got := strings.Contains(string(text), crashLine); got != (level == "debug") {
						t.Errorf("%s at %s: the backend's own line is there: %v", where, level, got)
					}
				}
				check("the log", log)
				check("the bundle", bundleEvents(t, ctx, g))
			})
		}
	}
}

// exitReasonIn finds the runtime's exit event, carrying how the process ended.
func exitReasonIn(t *testing.T, events []byte, runtime string) bool {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(string(events)))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e struct {
			Name    string `json:"eventName"`
			Runtime string `json:"runtime"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // the log also has plain lines; the bundle has events only
		}
		if e.Name == "runtime.exited" && e.Runtime == runtime && strings.Contains(e.Error.Message, "exit status 1") {
			return true
		}
	}
	return false
}

// bundleEvents runs the real diagnostics command and returns logs/events.jsonl from its archive.
func bundleEvents(t *testing.T, ctx context.Context, g *lab.Gateway) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	cmd := exec.CommandContext(ctx, filepath.Join(g.W.Bin, "local-base"), "--root", g.Root, "--json", "diagnostics", "--output", path)
	cmd.Env = g.CLIEnv() // never this machine's own home
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("diagnostics: %v: %.500s", err, out)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "logs/events.jsonl" {
			continue
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatal("the bundle has no logs/events.jsonl")
	return nil
}
