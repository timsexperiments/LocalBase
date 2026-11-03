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
// Exit 0: everything passed. Exit 1: findings about the pages, in result.md with captures.
// Exit 2: the suite itself could not run. Exit 4: it ran, and something it should have checked
// has no record of running, which result.md lists under "What ran". Exit 5: a journey failed,
// or the gateway panicked or logged a key.
package main

import (
	"bufio"
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
	"regexp"
	"slices"
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

// exitBroken is a journey that failed, or a gateway that panicked or logged a key. It is apart
// from exit 1 because a workflow may tolerate findings about layout while it must not tolerate
// these. run.ts returns the same status for a failed journey.
const exitBroken = 5

func main() {
	out := flag.String("out", "", "result directory (default: a new directory under the scratch parent, in localbase-e2e)")
	chrome := flag.String("chrome", os.Getenv("LBGO_CHROME"), "Chrome executable")
	axe := flag.String("axe", "", "the pinned axe-core script")
	keep := flag.Bool("keep", false, "keep the scratch root and logs")
	var p pins
	flag.StringVar(&p.chromeDir, "chrome-dir", "", "where the pinned Chrome is unpacked; with it, Chrome is checked before it runs")
	flag.StringVar(&p.chromeZip, "chrome-archive", "", "the archive that Chrome came from")
	flag.StringVar(&p.chromeSum, "chrome-sha256", "", "the archive's pinned sha256")
	flag.StringVar(&p.chromeVersion, "chrome-version", "", "the pinned Chrome's version")
	flag.StringVar(&p.axeZip, "axe-archive", "", "the tarball that axe-core came from")
	flag.StringVar(&p.axeSum, "axe-sha256", "", "the tarball's pinned sha256")
	flag.Parse()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	if err := p.verify(*axe); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: refusing to run what is in the cache: %v\ne2e: remove it, and tools/e2e.sh fetches it again\n", err)
		os.Exit(2)
	}
	os.Exit(run(*out, *chrome, *axe, *keep))
}

// pins are what a fetched browser and axe-core must match. With a Chrome from LBGO_CHROME or from
// the machine there is nothing to match, and result.md says so.
type pins struct {
	chromeDir, chromeZip, chromeSum, chromeVersion, axeZip, axeSum string
}

// verify checks them before anything from the cache runs, and tells run.ts what it found, for
// result.md.
func (p pins) verify(axe string) error {
	if p.axeZip != "" {
		if err := verifyAxe(axe, p.axeZip, p.axeSum); err != nil {
			return err
		}
		os.Setenv("E2E_AXE_SHA256", p.axeSum)
	}
	if p.chromeDir == "" {
		return nil
	}
	files, err := verifyChrome(p.chromeDir, p.chromeZip, p.chromeSum)
	if err != nil {
		return err
	}
	os.Setenv("E2E_CHROME_SHA256", p.chromeSum)
	os.Setenv("E2E_CHROME_VERSION", p.chromeVersion)
	os.Setenv("E2E_CHROME_FILES", strconv.Itoa(files))
	return nil
}

func run(out, chrome, axe string, keep bool) (code int) {
	if chrome == "" || axe == "" {
		fmt.Fprintln(os.Stderr, "e2e: needs -chrome and -axe; use tools/e2e.sh")
		return 2
	}
	// LOCALBASE_E2E_SCRATCH keeps a run's scratch in one place of the caller's, so that clearing up
	// never means reaching outside it.
	parent := os.Getenv("LOCALBASE_E2E_SCRATCH")
	if parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			return 2
		}
	}
	if out == "" {
		root := parent
		if root == "" {
			root = os.TempDir()
		}
		out = filepath.Join(root, "localbase-e2e", time.Now().UTC().Format("20060102T150405Z"))
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
	w, err := lab.NewWorkspace(parent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	w.Keep = keep
	// Findings are in the result with their captures. Only a suite that could not finish
	// needs what is in the scratch root to work out why, however early it stopped.
	defer func() {
		// A planted failure explains itself, and tools/e2e-manifest-check.sh ends that way every time.
		planted := code == exitBroken && os.Getenv("E2E_PLANT_OFF") != ""
		if code > 1 && code != exitIncomplete && !planted {
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
	if plantOn("logged-key") {
		if f, err := os.OpenFile(g.LogPath, os.O_APPEND|os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "{\"planted\":\"%s\"}\n", plantKey())
			f.Close()
		}
	}
	if problem := checkLog(g.LogPath, append([]string{g.Keys.User, g.Keys.Admin}, g.Keys.Runs...)); problem != "" {
		fmt.Fprintln(os.Stderr, "e2e:", problem)
		noteInResult(out, problem)
		if code < 2 {
			code = exitBroken
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

// keyShape is what an API key looks like (store.newRawKey). The check looks for the shape as
// well as this run's keys, since no key of any kind belongs in the log.
var keyShape = regexp.MustCompile(`lb_[A-Za-z0-9_-]{32}`)

// checkLog says what is wrong with the gateway's log, or "". It goes straight to the run's output,
// so what it says about a key is the file and the line, and never anything from that line.
func checkLog(path string, secrets []string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	// A panic's own words help, unless they could hold a key or part of one.
	says := func(text string) string {
		if strings.Contains(text, "lb_") {
			return ""
		}
		return ": " + text
	}
	problem := ""
	r := bufio.NewReaderSize(f, 256<<10)
	for n := 1; ; n++ {
		line, readErr := r.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		if keyShape.MatchString(line) {
			return fmt.Sprintf("an API key is in the log: %s line %d", path, n)
		}
		for _, s := range secrets {
			if s != "" && strings.Contains(line, s) {
				return fmt.Sprintf("an API key is in the log: %s line %d", path, n)
			}
		}
		var e lab.Event
		switch {
		case strings.HasPrefix(line, "panic:"), strings.HasPrefix(line, "fatal error:"):
			problem = fmt.Sprintf("the gateway panicked: %s line %d%s", path, n, says(line))
		case strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &e) == nil && e.Name == "http.panic":
			problem = fmt.Sprintf("a handler panicked: %s line %d%s", path, n, says(e.Message))
		}
		if readErr != nil {
			return problem // keeps reading to the end, since a key outranks a panic
		}
	}
}

// plantOn says a run was asked to fail in this way, to show that it does (tools/e2e-manifest-check.sh).
func plantOn(name string) bool {
	return slices.Contains(strings.Split(os.Getenv("E2E_PLANT_OFF"), ","), name)
}

// plantKey is a key that was never issued. It is put together here so that it is not a key-shaped
// string in the repository for a secret scanner to find.
func plantKey() string { return "lb_" + strings.Repeat("Zq9x", 8) }

// noteInResult makes result.md say what the exit status says, since the log is read after it is written.
func noteInResult(out, problem string) {
	path := filepath.Join(out, "result.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return // the suite did not get as far as writing one
	}
	text := string(b)
	for _, title := range []string{"# Browser suite: PASS", "# Browser suite: FINDINGS"} {
		text = strings.Replace(text, title, "# Browser suite: GATEWAY LOG FAILED", 1)
	}
	text += "\n## Gateway log\n\n**Failed:** " + problem + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
	}
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
