// Command e2e runs the browser suite against a real gateway over fake backends, in
// headless Chrome: the journeys a person takes (signing in with a first-run link, a
// first chat, the Studio tools, what happens when things break) and a matrix of pages,
// widths and themes checked for overflow, accessibility, focus, keyboard use and
// reduced motion.
//
//	make e2e
//
// Use tools/e2e.sh rather than running this directly: it finds or fetches a pinned
// Chrome, and gives a run its own session and a hard stop. The checks themselves are in
// run.ts. See docs/reliability.md.
//
// Exit 0: everything passed. Exit 1: findings, in result.md with captures. Exit 2: the
// suite itself could not run. Exit 4: it ran, and something it should have checked has no
// record of running, which result.md lists under "What ran".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// exitIncomplete is run.ts's status for a run that finished and left something unchecked. Its
// scratch explains nothing that result.md does not, and tools/e2e-manifest-check.sh ends this
// way every time, so it is not kept.
const exitIncomplete = 4

func main() {
	out := flag.String("out", "", "result directory (default: a new directory under $TMPDIR/localbase-e2e)")
	chrome := flag.String("chrome", os.Getenv("LBGO_CHROME"), "Chrome executable")
	axe := flag.String("axe", "", "the pinned axe-core script")
	keep := flag.Bool("keep", false, "keep the scratch root and logs")
	flag.Parse()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	os.Exit(run(*out, *chrome, *axe, *keep))
}

func run(out, chrome, axe string, keep bool) (code int) {
	if chrome == "" || axe == "" {
		fmt.Fprintln(os.Stderr, "e2e: needs -chrome and -axe; use tools/e2e.sh")
		return 2
	}
	if out == "" {
		out = filepath.Join(os.TempDir(), "localbase-e2e", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	const hard = 25 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), hard)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	w, err := lab.NewWorkspace("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	w.Keep = keep
	// Findings are in the result with their captures. Only a suite that could not finish
	// needs what is in the scratch root to work out why, however early it stopped.
	defer func() {
		if code > 1 && code != exitIncomplete {
			w.Keep = true
			fmt.Fprintf(os.Stderr, "e2e: scratch kept at %s\n", w.Dir)
		}
		w.Close()
	}()
	// The last resort, for a suite stuck past its deadline. tools/e2e-kill-check.sh brings it
	// forward to show that it leaves nothing running.
	forceExit := hard + 30*time.Second
	if v, err := strconv.Atoi(os.Getenv("LOCALBASE_E2E_FORCE_EXIT_SECONDS")); err == nil && v > 0 {
		forceExit = time.Duration(v) * time.Second
	}
	time.AfterFunc(forceExit, func() {
		fmt.Fprintln(os.Stderr, "e2e: hard deadline passed; stopping everything")
		w.KillStray()
		os.Exit(2)
	})
	if err := w.Build(ctx, false); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	g, err := w.Start(ctx, lab.Options{Name: "gw", CtxSize: 8192, FakeDelayMS: 20})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	// A gateway that will not stop is a failure, whatever the pages did.
	defer func() {
		if _, err := g.Stop(30 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: the gateway did not stop cleanly:", err)
			if code == 0 {
				code = 1
			}
		}
	}()

	control, err := startControl(ctx, w, g)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	stopChrome, cdp, err := startChrome(ctx, w, chrome)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	defer stopChrome()

	wav := filepath.Join(w.Dir, "speech.wav")
	if err := os.WriteFile(wav, lab.WAV(16000), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	module, err := moduleDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	cmd := exec.CommandContext(ctx, "bun", "run", filepath.Join(module, "tools", "e2e", "run.ts"), cdp, g.Base, control, out, axe, wav)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if err != nil {
		code = 2
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() >= 0 {
			code = exit.ExitCode()
		}
	}
	// Whatever the pages did, the gateway must not have panicked or logged a key.
	if problem := checkLog(g); problem != "" {
		fmt.Fprintln(os.Stderr, "e2e:", problem)
		if code == 0 {
			code = 1
		}
	}
	fmt.Fprintf(os.Stderr, "e2e: result %s\n", filepath.Join(out, "result.md"))
	return code
}

func moduleDir() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

func checkLog(g *lab.Gateway) string {
	secrets := append([]string{g.Keys.User, g.Keys.Admin}, g.Keys.Runs...)
	problem := ""
	tail := &lab.LogTail{Path: g.LogPath, Keep: func(lab.Event) bool { return false },
		Line: func(line string) {
			for _, s := range secrets {
				if strings.Contains(line, s) {
					problem = "an API key is in the log"
				}
			}
		},
		Seen: func(e lab.Event) {
			if e.Name == "http.panic" {
				problem = "a handler panicked: " + e.Message
			}
		},
		Raw: func(line string) {
			if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
				problem = line
			}
		}}
	if err := tail.Read(); err != nil {
		return err.Error()
	}
	return problem
}

// startControl serves what the page script cannot do itself: get a sign-in link, end
// sessions, change the host's memory, kill a backend. Loopback only, in the lab's block,
// and gone with the run.
func startControl(ctx context.Context, w *lab.Workspace, g *lab.Gateway) (string, error) {
	port, err := w.TakePort()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	c := lab.NewClient(g.Base)
	mux := http.NewServeMux()
	fail := func(rw http.ResponseWriter, err error) { http.Error(rw, err.Error(), 500) }
	mux.HandleFunc("POST /link", func(rw http.ResponseWriter, r *http.Request) {
		out, err := g.CLI(r.Context(), "--json", "access", "login-link")
		if err != nil {
			fail(rw, fmt.Errorf("%w: %s", err, out))
			return
		}
		rw.Write(out)
	})
	mux.HandleFunc("POST /revoke", func(rw http.ResponseWriter, r *http.Request) {
		if out, err := g.CLI(r.Context(), "--json", "access", "login-link", "--revoke"); err != nil {
			fail(rw, fmt.Errorf("%w: %s", err, out))
		}
	})
	mux.HandleFunc("POST /host", func(rw http.ResponseWriter, r *http.Request) {
		avail, err := strconv.ParseFloat(r.URL.Query().Get("available"), 64)
		if err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		pressure := r.URL.Query().Get("pressure")
		if pressure == "" {
			pressure = "normal"
		}
		if err := g.Fake.Set(avail, pressure); err != nil {
			fail(rw, err)
		}
	})
	mux.HandleFunc("POST /kill", func(rw http.ResponseWriter, r *http.Request) {
		procs, err := w.Backends()
		if err != nil {
			fail(rw, err)
			return
		}
		killed := 0
		for _, p := range procs {
			if p.Command == r.URL.Query().Get("name") {
				if err := syscall.Kill(p.PID, syscall.SIGKILL); err == nil {
					killed++
				}
			}
		}
		_ = json.NewEncoder(rw).Encode(map[string]int{"killed": killed})
	})
	mux.HandleFunc("POST /unload", func(rw http.ResponseWriter, r *http.Request) {
		res := c.Do(r.Context(), "POST", "/_localbase/models/"+r.URL.Query().Get("model")+"/unload", g.Keys.Admin, "", nil)
		rw.WriteHeader(res.Status)
		rw.Write(res.Body)
	})
	mux.HandleFunc("GET /models", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]string{"chat": lab.ChatSmall, "large": lab.ChatLarge, "image": lab.Image, "tts": lab.TTS, "stt": lab.STT, "decision": lab.Decision})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }() // ends with ErrServerClosed
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

// lastLines is the end of a file, for an error that would otherwise only point at it.
func lastLines(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(could not read it: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "(nothing)"
	}
	return strings.Join(lines, "\n")
}

// chromeArgs is how Chrome starts. The first two flags keep it away from the machine's
// keychain: without them a current Chrome on macOS asks the Keychain for its cookie key,
// which can stall navigation for good and put a prompt on the screen of whoever owns the
// machine. They apply to any Chrome, including one named by LBGO_CHROME.
func chromeArgs(port int, profile string, noSandbox bool) []string {
	args := []string{"--use-mock-keychain", "--password-store=basic",
		"--headless=new", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--disable-background-networking",
		"--disable-sync", "--disable-extensions", "--metrics-recording-only", "--mute-audio"}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	return append(args, "about:blank")
}

// startChrome runs headless Chrome with a scratch profile and returns its DevTools
// address once it answers.
func startChrome(ctx context.Context, w *lab.Workspace, chrome string) (func(), string, error) {
	port, err := w.TakePort()
	if err != nil {
		return nil, "", err
	}
	profile := filepath.Join(w.Dir, "chrome-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, "", err
	}
	cmd := exec.Command(chrome, chromeArgs(port, profile, os.Getenv("LOCALBASE_E2E_NO_SANDBOX") != "")...)
	// Its own process group, so its helpers go with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(w.Dir, "chrome.log"))
	if err != nil {
		return nil, "", err
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting Chrome: %w", err)
	}
	// A SIGKILL or the hard deadline skips every deferred stop.
	w.StopIfThisProcessDies(cmd.Process.Pid)
	stop := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) // it may have gone already
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }() // a signal's exit status is expected
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		log.Close()
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if resp, err := client.Get(base + "/json/version"); err == nil {
			resp.Body.Close()
			return stop, base, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			stop()
			return nil, "", fmt.Errorf("Chrome did not answer on its DevTools port; %s ends:\n%s", log.Name(), lastLines(log.Name(), 20))
		}
	}
}
