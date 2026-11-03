package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/tools/lab"
)

// drainBudget bounds a SIGTERM under load: serve allows 5 s for runs and 10 s
// for connections, and a stop gets its own grace.
const drainBudget = 25 * time.Second

type drainResult struct {
	ExitOK       bool          `json:"exitOk"`
	Took         time.Duration `json:"took"`
	HealthAfter  string        `json:"healthAfter"`
	ShortStreams int           `json:"shortStreamsCompleted"`
	LongCut      bool          `json:"longStreamCut"`
	RestartTook  time.Duration `json:"restartTook"`
}

// drainAndRestart stops the gateway under load and starts it again on the same
// root. In-flight work finishes, a run survives as interrupted with its partial
// output, nothing is left running, and the restart serves requests.
func (s *soak) drainAndRestart(ctx context.Context, stopTraffic context.CancelFunc, traffic *sync.WaitGroup) {
	p := &phase{name: "drain", avail: ampleGiB, pressure: "normal", allow: allow(codeStopping, codeMemory), cuts: true, drain: true}
	s.enter(s.cycles, p)
	s.gate.llm.Store(true)
	if !settle(ctx, &s.gate.llmInflight, requestLimit) {
		s.addCheck("drain", false, "LLM clients did not settle")
	}
	if !s.loadSmall(ctx) {
		return
	}
	// The slot is taken, two short streams wait behind it, and a run and one
	// stream too long for any drain are queued after them.
	key := s.g.Keys.Runs[0]
	bg, stopProbes := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopProbes()
	type outcome struct {
		long bool
		st   lab.Stream
	}
	results := make(chan outcome, 4)
	stream := func(words int, long bool, started chan<- struct{}) {
		body := chatBody(lab.ChatSmall, fmt.Sprintf("fake:slow %d", words))
		body["stream"] = true
		st := s.c.ChatStream(bg, s.g.Keys.User, body, func(*lab.Stream) bool {
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			return true
		})
		results <- outcome{long, st}
	}
	started := make(chan struct{}, 1)
	go stream(30, false, started)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		s.addCheck("drain", false, "the first stream never started")
	}
	for i := 1; i <= 2; i++ {
		go stream(20, false, nil)
		waitQueued(ctx, s, i)
	}
	create := s.c.PostJSON(ctx, "/_localbase/runs/v1/chat/completions", key, chatBody(lab.ChatSmall, "fake:slow 400"))
	var run runView
	if create.Status != 202 || json.Unmarshal(create.Body, &run) != nil || run.ID == "" {
		s.addCheck("drain", false, "the run did not start: %d %.200s", create.Status, create.Body)
	}
	waitQueued(ctx, s, 3)
	go stream(1200, true, nil)
	waitQueued(ctx, s, 4)

	began := time.Now()
	if err := s.g.Signal(syscall.SIGTERM); err != nil {
		s.addCheck("drain", false, "SIGTERM: %v", err)
	}
	// While it stops, health says so or the port is closed; it never says ok
	// for long.
	s.drain.HealthAfter = healthDuringStop(s.g.Base)
	select {
	case <-s.g.Done():
	case <-time.After(drainBudget + 15*time.Second):
		s.addCheck("SIGTERM exits in budget", false, "not stopped after %v", time.Since(began))
		_ = s.g.Signal(syscall.SIGKILL)
		<-s.g.Done()
	}
	s.drain.Took = time.Since(began)
	s.drain.ExitOK = s.g.ExitErr() == nil
	s.addCheck("SIGTERM exits 0", s.drain.ExitOK, "exit: %v", s.g.ExitErr())
	s.addCheck("SIGTERM exits in budget", s.drain.Took <= drainBudget, "%v (budget %v)", s.drain.Took.Round(time.Millisecond), drainBudget)
	stopTraffic()
	traffic.Wait()
	s.cur.Store(p)

	for range 4 {
		o := <-results
		if o.long {
			s.drain.LongCut = !o.st.Done
			if o.st.Done {
				s.addCheck("a stream past the drain is cut", false, "it finished; expected the deadline to end it")
			}
			continue
		}
		if o.st.Status == 200 && o.st.Done && o.st.Err == nil {
			s.drain.ShortStreams++
		} else {
			s.addCheck("in-flight work finishes", false, "a short stream: status %d code %s err %v done %v", o.st.Status, o.st.Code, o.st.Err, o.st.Done)
		}
	}
	s.addCheck("in-flight work finishes", s.drain.ShortStreams == 3, "%d of 3 short streams completed", s.drain.ShortStreams)

	// Nothing is left running.
	deadline := time.Now().Add(12 * time.Second)
	var left []lab.Proc
	for time.Now().Before(deadline) {
		left, _ = s.w.Backends()
		if len(left) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.addCheck("no backend outlives the gateway", len(left) == 0, "%s", procNames(left))
	if runtimeStoppedAfterGateway(s.g.LogPath) {
		s.addCheck("runtimes stop before gateway.stopped", false, "the log says stopped while a runtime is still running")
	} else {
		s.addCheck("runtimes stop before gateway.stopped", true, "")
	}
	s.checkRunFiles()

	// The restart on the same root.
	s.readLog()
	restartBegan := time.Now()
	g2, err := s.w.Start(ctx, lab.Options{Name: "gw", Reuse: s.g, QueueCapacity: queueCapacity, QueueTimeoutMS: int(queueTimeout / time.Millisecond),
		Parallel: "1", FakeDelayMS: int(fakeDelay / time.Millisecond), CtxSize: 8192})
	if err != nil {
		s.addCheck("restart on the same root", false, "%v", err)
		return
	}
	s.drain.RestartTook = time.Since(restartBegan)
	defer func() {
		if took, err := g2.Stop(30 * time.Second); err != nil {
			s.addCheck("the restarted gateway stops", false, "after %v: %v", took, err)
		}
	}()
	c2 := lab.NewClient(g2.Base)
	s.g2 = g2
	if run.ID != "" {
		view := c2.Do(ctx, "GET", "/_localbase/runs/"+run.ID, key, "", nil)
		var v runView
		ok := view.Status == 200 && json.Unmarshal(view.Body, &v) == nil && v.Status == "interrupted" && v.Reason == "gateway_restart"
		s.addCheck("a run survives SIGTERM as interrupted", ok, "%s", detailUnless(ok, view))
		events := c2.Do(ctx, "GET", "/_localbase/runs/"+run.ID+"/events", key, "", nil)
		ok = events.Status == 200 && strings.Contains(string(events.Body), "event: snapshot") && strings.Contains(string(events.Body), "event: end")
		s.addCheck("a restored run's events end cleanly", ok, "%s", detailUnless(ok, events))
	}
	chat := chatContent(c2.PostJSON(ctx, "/v1/chat/completions", g2.Keys.User, chatBody(lab.ChatSmall, canary+" hello")), lab.ChatSmall)
	s.addCheck("the restart serves requests", chat.Status == 200 && chat.Invalid == "" && chat.Err == nil, "%d %v %s", chat.Status, chat.Err, chat.Invalid)
}

// detailUnless is a failed check's evidence; a passing one needs none.
func detailUnless(ok bool, c lab.Call) string {
	if ok {
		return ""
	}
	return fmt.Sprintf("%d %.300s", c.Status, c.Body)
}

func waitQueued(ctx context.Context, s *soak, n int) {
	deadline := time.Now().Add(3 * time.Second)
	for s.queued(ctx) < n && time.Now().Before(deadline) {
		sleepCtx(ctx, 5*time.Millisecond)
	}
}

// healthDuringStop follows /health from the SIGTERM on, returning how it went:
// the statuses in order of first appearance.
func healthDuringStop(base string) string {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	seen := []string{}
	note := func(s string) {
		if len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
		}
	}
	for deadline := time.Now().Add(time.Second + 5*time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		resp, err := client.Get(base + "/health")
		if err != nil {
			note("refused")
			break
		}
		resp.Body.Close()
		note(fmt.Sprint(resp.StatusCode))
	}
	return strings.Join(seen, " > ")
}

// runtimeStoppedAfterGateway reports whether any runtime stopped after the log
// said the gateway had.
func runtimeStoppedAfterGateway(path string) bool {
	stopped, late := false, false
	tail := &lab.LogTail{Path: path, Keep: func(lab.Event) bool { return false }, Seen: func(e lab.Event) {
		switch e.Name {
		case "gateway.stopped":
			stopped = true
		case "runtime.stopped":
			late = late || stopped
		}
	}}
	return tail.Read() != nil || late
}

// checkRunFiles reads the checkpoints a stopped gateway left: none may still
// claim to be running.
func (s *soak) checkRunFiles() {
	paths, _ := filepath.Glob(filepath.Join(s.g.Root, "runs", "*.json"))
	unsettled := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &rec) == nil && (rec.Status == "queued" || rec.Status == "in_progress") {
			unsettled++
		}
	}
	s.addCheck("no checkpoint claims to be running after a stop", unsettled == 0, "%d of %d", unsettled, len(paths))
}
